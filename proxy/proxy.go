package proxy

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-gl/mathgl/mgl32"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"github.com/Olavo635/plproxy/lua"
	"golang.org/x/oauth2"
)

const localAddress = "0.0.0.0:19132"

// Server representa um servidor destino.
type Server struct {
	Name    string
	Address string
}

// Proxy é o proxy principal do PL Proxy.
type Proxy struct {
	src    oauth2.TokenSource
	server Server
	engine *lua.Engine
}

func New(src oauth2.TokenSource, server Server, engine *lua.Engine) *Proxy {
	return &Proxy{src: src, server: server, engine: engine}
}

func (p *Proxy) Start() error {
	status, err := minecraft.NewForeignStatusProvider(p.server.Address)
	if err != nil {
		return fmt.Errorf("status provider: %w", err)
	}

	listener, err := minecraft.ListenConfig{
		StatusProvider: status,
	}.Listen("raknet", localAddress)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()

	fmt.Printf("[proxy] Escutando em %s → %s\n", localAddress, p.server.Address)
	fmt.Println("[proxy] Conecte seu Minecraft Bedrock no endereço: 127.0.0.1:19132")
	fmt.Println("[proxy] Use .run <arquivo.lua> para carregar scripts")

	for {
		conn, err := listener.Accept()
		if err != nil {
			return fmt.Errorf("accept: %w", err)
		}
		go p.handleConn(conn.(*minecraft.Conn), listener)
	}
}

func (p *Proxy) handleConn(client *minecraft.Conn, listener *minecraft.Listener) {
	serverConn, err := minecraft.Dialer{
		TokenSource: p.src,
		ClientData:  client.ClientData(),
	}.Dial("raknet", p.server.Address)
	if err != nil {
		log.Printf("[proxy] Falha ao conectar no servidor: %v", err)
		_ = client.Close()
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var e1, e2 error
	go func() { defer wg.Done(); e1 = client.StartGame(serverConn.GameData()) }()
	go func() { defer wg.Done(); e2 = serverConn.DoSpawn() }()
	wg.Wait()

	if e1 != nil || e2 != nil {
		log.Printf("[proxy] Erro no spawn: client=%v server=%v", e1, e2)
		_ = client.Close()
		_ = serverConn.Close()
		return
	}

	fmt.Println("[proxy] Jogador conectado!")
	sess := newSession(client, serverConn, listener, p.server, p.engine)
	p.engine.SetSession(sess)
	sess.run()
}

// ─────────────────────────────────────────────────────────────
//  session
// ─────────────────────────────────────────────────────────────

type session struct {
	client   *minecraft.Conn
	server   *minecraft.Conn
	listener *minecraft.Listener
	srvInfo  Server
	engine   *lua.Engine

	mu        sync.Mutex
	stopping  atomic.Bool
	stopTimer *time.Timer

	connectedAt time.Time

	// posição e gamemode
	savedGameMode   int32
	savedPosition   mgl32.Vec3
	savedYaw        float32
	savedPitch      float32
	entityRuntimeID uint64
}

func newSession(client, server *minecraft.Conn, listener *minecraft.Listener, srv Server, engine *lua.Engine) *session {
	gd := server.GameData()
	return &session{
		client:          client,
		server:          server,
		listener:        listener,
		srvInfo:         srv,
		engine:          engine,
		connectedAt:     time.Now(),
		savedGameMode:   gd.PlayerGameMode,
		entityRuntimeID: gd.EntityRuntimeID,
	}
}

// ─────────────────────────────────────────────────────────────
//  Interface Session (para o engine Lua)
// ─────────────────────────────────────────────────────────────

func (s *session) SendMessage(msg string) {
	s.sendMessage(msg)
}

func (s *session) GetPosition() mgl32.Vec3 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.savedPosition
}

func (s *session) SetPosition(pos mgl32.Vec3) {
	s.mu.Lock()
	defer s.mu.Unlock()

	newPos := pos

	// Teleporta no servidor
	_ = s.server.WritePacket(&packet.MovePlayer{
		EntityRuntimeID: s.entityRuntimeID,
		Position:        newPos,
		Mode:            packet.MoveModeTeleport,
	})
	// Atualiza cliente também
	_ = s.client.WritePacket(&packet.MovePlayer{
		EntityRuntimeID: s.entityRuntimeID,
		Position:        newPos,
		Mode:            packet.MoveModeTeleport,
	})

	s.savedPosition = newPos
}

func (s *session) GetYaw() float32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.savedYaw
}

func (s *session) GetPitch() float32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.savedPitch
}

func (s *session) GetGamemode() int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.savedGameMode
}

func (s *session) GetEntityRuntimeID() uint64 {
	return s.entityRuntimeID
}

func (s *session) GetServerName() string {
	return s.srvInfo.Name
}

func (s *session) GetServerAddress() string {
	return s.srvInfo.Address
}

func (s *session) SendPacketToClient(pk packet.Packet) error {
	return s.client.WritePacket(pk)
}

func (s *session) SendPacketToServer(pk packet.Packet) error {
	return s.server.WritePacket(pk)
}

// ─────────────────────────────────────────────────────────────
//  Loop principal
// ─────────────────────────────────────────────────────────────

func (s *session) run() {
	done := make(chan struct{}, 2)

	// Cliente → Servidor
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			pk, err := s.client.ReadPacket()
			if err != nil {
				return
			}
			if s.handleClientPacket(pk) {
				continue
			}
			// Executa hooks Lua do cliente
			if s.engine.ExecuteClientHooks(s, pk) {
				continue
			}
			if err := s.server.WritePacket(pk); err != nil {
				return
			}
		}
	}()

	// Servidor → Cliente
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			pk, err := s.server.ReadPacket()
			if err != nil {
				return
			}
			if s.handleServerPacket(pk) {
				continue
			}
			// Executa hooks Lua do servidor
			if s.engine.ExecuteServerHooks(s, pk) {
				continue
			}
			if err := s.client.WritePacket(pk); err != nil {
				return
			}
		}
	}()

	<-done
	_ = s.client.Close()
	_ = s.server.Close()
}

// handleClientPacket — retorna true se o pacote foi consumido pelo proxy.
func (s *session) handleClientPacket(pk packet.Packet) bool {
	switch p := pk.(type) {
	case *packet.Text:
		if p.TextType == packet.TextTypeChat && strings.HasPrefix(p.Message, ".") {
			s.handleCommand(p.Message)
			return true
		}
	case *packet.MovePlayer:
		s.mu.Lock()
		s.savedPosition = p.Position
		s.savedYaw = p.HeadYaw
		s.savedPitch = p.Pitch
		s.mu.Unlock()
	case *packet.PlayerAuthInput:
		s.mu.Lock()
		s.savedPosition = p.Position
		s.mu.Unlock()
	}
	return false
}

// handleServerPacket — retorna true se o pacote deve ser bloqueado.
func (s *session) handleServerPacket(pk packet.Packet) bool {
	switch p := pk.(type) {
	case *packet.SetPlayerGameType:
		s.mu.Lock()
		s.savedGameMode = p.GameType
		s.mu.Unlock()
	case *packet.NetworkStackLatency:
		// Sempre consome pacotes de latência
		return true
	}
	return false
}

// ─────────────────────────────────────────────────────────────
//  Roteador de comandos
// ─────────────────────────────────────────────────────────────

func (s *session) handleCommand(raw string) {
	parts := strings.Fields(raw)
	if len(parts) == 0 {
		return
	}

	cmdName := strings.ToLower(parts[0])
	args := parts[1:]

	switch cmdName {
	case ".help", ".ajuda":
		s.sendHelp()
	case ".run":
		s.handleRun(args)
	case ".debug":
		s.handleDebug(args)
	case ".ids":
		s.handleIds()
	case ".stop":
		s.handleStopCommand(args)
	default:
		// Procura nos comandos registrados pelos scripts Lua
		cmd := s.engine.GetCommand(strings.TrimPrefix(cmdName, "."))
		if cmd != nil {
			cmd.Handler(s, args)
		} else {
			s.sendMessage(fmt.Sprintf("§cComando desconhecido: §e%s §c— use §e.ajuda§c para ver a lista.", cmdName))
		}
	}
}

// ─────────────────────────────────────────────────────────────
//  .ajuda
// ─────────────────────────────────────────────────────────────

func (s *session) sendHelp() {
	lines := s.engine.GetHelp()
	for _, l := range lines {
		s.sendMessage(l)
	}
}

// ─────────────────────────────────────────────────────────────
//  .run <arquivo.lua>
// ─────────────────────────────────────────────────────────────

func (s *session) handleRun(args []string) {
	if len(args) == 0 {
		s.sendMessage("§c[Uso] §f.run §e<arquivo.lua>")
		return
	}

	path := args[0]
	if !strings.HasSuffix(path, ".lua") {
		path += ".lua"
	}

	id, err := s.engine.RunFile(path, s)
	if err != nil {
		s.sendMessage(fmt.Sprintf("§c[Script] §fErro ao carregar §e%s§f: §c%v", path, err))
		return
	}

	s.sendMessage(fmt.Sprintf("§a[Script] §fScript §e%s§f carregado! §aID: #%d", path, id))
}

// ─────────────────────────────────────────────────────────────
//  .debug <arquivo.lua>
// ─────────────────────────────────────────────────────────────

func (s *session) handleDebug(args []string) {
	if len(args) == 0 {
		s.sendMessage("§c[Uso] §f.debug §e<arquivo.lua>")
		return
	}

	path := args[0]
	if !strings.HasSuffix(path, ".lua") {
		path += ".lua"
	}

	id, err := s.engine.RunFileDebug(path, s)
	if err != nil {
		s.sendMessage(fmt.Sprintf("§c[Debug] §fErro ao carregar §e%s§f: §c%v", path, err))
		return
	}

	s.sendMessage(fmt.Sprintf("§a[Debug] §fScript §e%s§f em execução! §aID: #%d", path, id))
}

// ─────────────────────────────────────────────────────────────
//  .ids
// ─────────────────────────────────────────────────────────────

func (s *session) handleIds() {
	scripts := s.engine.GetActiveScripts()

	if len(scripts) == 0 {
		s.sendMessage("§e[Scripts] §fNenhum script ativo.")
		return
	}

	s.sendMessage("§b§l╔══════════════════════════════════════╗")
	s.sendMessage("§b§l║         §eScripts Ativos                  §b§l║")
	s.sendMessage("§b§l╠══════════════════════════════════════╣")
	for _, sc := range scripts {
		s.sendMessage(fmt.Sprintf(
			"§b§l║ §a#%d §f%-20s §7Cmds:%d Hooks:%d",
			sc.ID, sc.Name, sc.Commands, sc.Hooks,
		))
	}
	s.sendMessage("§b§l╠══════════════════════════════════════╣")
	s.sendMessage("§b§l║ §e.stop §7<id> §fpara parar um script")
	s.sendMessage("§b§l╚══════════════════════════════════════╝")
}

// ─────────────────────────────────────────────────────────────
//  .stop [id]
// ─────────────────────────────────────────────────────────────

func (s *session) handleStopCommand(args []string) {
	// .stop sem argumentos = parar o proxy
	if len(args) == 0 {
		s.handleStopProxy()
		return
	}

	// .stop <id> = parar script
	idStr := args[0]
	id, err := strconv.Atoi(idStr)
	if err != nil {
		s.sendMessage(fmt.Sprintf("§c[Stop] §fID inválido: §e%s§f. Use §e.ids§f para ver os IDs.", idStr))
		return
	}

	if !s.engine.IsScriptRunning(id) {
		s.sendMessage(fmt.Sprintf("§c[Stop] §fScript §e#%d§f não está rodando.", id))
		return
	}

	if s.engine.StopScript(id) {
		s.sendMessage(fmt.Sprintf("§a[Stop] §fScript §e#%d§f parado com sucesso!", id))
	} else {
		s.sendMessage(fmt.Sprintf("§c[Stop] §fErro ao parar script §e#%d§f.", id))
	}
}

// ─────────────────────────────────────────────────────────────
//  .stop (parar o proxy)
// ─────────────────────────────────────────────────────────────

func (s *session) handleStopProxy() {
	if !s.stopping.Load() {
		s.stopping.Store(true)
		s.sendMessage("§e[Stop] §fDigite §c.stop§f novamente em §c10 segundos§f para confirmar.")
		s.stopTimer = time.AfterFunc(10*time.Second, func() {
			s.stopping.Store(false)
			s.sendMessage("§e[Stop] §fCancelado — tempo expirado.")
		})
		return
	}
	if s.stopTimer != nil {
		s.stopTimer.Stop()
	}
	s.sendMessage("§c[Stop] §fEncerrando o proxy... Até mais!")
	s.engine.Clear()
	time.Sleep(500 * time.Millisecond)
	_ = s.client.Close()
	_ = s.server.Close()
	_ = s.listener.Close()
	log.Println("[proxy] Encerrado pelo jogador via .stop")
}

// ─────────────────────────────────────────────────────────────
//  Util
// ─────────────────────────────────────────────────────────────

func (s *session) sendMessage(msg string) {
	_ = s.client.WritePacket(&packet.Text{
		TextType: packet.TextTypeSystem,
		Message:  msg,
	})
}
