package proxy

import (
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-gl/mathgl/mgl32"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"golang.org/x/oauth2"
)

const (
	remoteAddress = "donutsmp.net:19132"
	localAddress  = "0.0.0.0:19132"
)

// Proxy é o proxy principal do Donut Proxy.
type Proxy struct {
	src oauth2.TokenSource
}

// New cria um novo Proxy.
func New(src oauth2.TokenSource) *Proxy {
	return &Proxy{src: src}
}

// Start inicia o listener local e começa a aceitar conexões.
func (p *Proxy) Start() error {
	status, err := minecraft.NewForeignStatusProvider(remoteAddress)
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

	fmt.Printf("[proxy] Escutando em %s → %s\n", localAddress, remoteAddress)
	fmt.Println("[proxy] Conecte seu Minecraft Bedrock no endereço: 127.0.0.1:19132")
	fmt.Println("[proxy] Digite .ajuda no chat para ver os comandos disponíveis")

	for {
		conn, err := listener.Accept()
		if err != nil {
			return fmt.Errorf("accept: %w", err)
		}
		go p.handleConn(conn.(*minecraft.Conn), listener)
	}
}

// handleConn gerencia uma conexão individual do cliente.
func (p *Proxy) handleConn(client *minecraft.Conn, listener *minecraft.Listener) {
	serverConn, err := minecraft.Dialer{
		TokenSource: p.src,
		ClientData:  client.ClientData(),
	}.Dial("raknet", remoteAddress)
	if err != nil {
		log.Printf("[proxy] Falha ao conectar no servidor: %v", err)
		_ = client.Close()
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var spawnErr1, spawnErr2 error
	go func() {
		defer wg.Done()
		spawnErr1 = client.StartGame(serverConn.GameData())
	}()
	go func() {
		defer wg.Done()
		spawnErr2 = serverConn.DoSpawn()
	}()
	wg.Wait()

	if spawnErr1 != nil {
		log.Printf("[proxy] Erro no spawn do cliente: %v", spawnErr1)
		_ = client.Close()
		_ = serverConn.Close()
		return
	}
	if spawnErr2 != nil {
		log.Printf("[proxy] Erro no spawn do servidor: %v", spawnErr2)
		_ = client.Close()
		_ = serverConn.Close()
		return
	}

	fmt.Println("[proxy] Jogador conectado!")

	session := newSession(client, serverConn, listener)
	session.run()
}

// session representa a sessão ativa de um jogador.
type session struct {
	client   *minecraft.Conn
	server   *minecraft.Conn
	listener *minecraft.Listener

	mu          sync.Mutex
	stopping    atomic.Bool
	stopTimer   *time.Timer

	// freecam
	freecamActive bool
	savedGameMode int32
	savedPosition mgl32.Vec3

	// fullbright
	fullbrightOn bool

	// runtime ID do jogador (do StartGame)
	entityRuntimeID uint64
}

func newSession(client *minecraft.Conn, server *minecraft.Conn, listener *minecraft.Listener) *session {
	gd := server.GameData()
	return &session{
		client:          client,
		server:          server,
		listener:        listener,
		savedGameMode:   gd.PlayerGameMode,
		entityRuntimeID: gd.EntityRuntimeID,
	}
}

// run inicia as goroutines de leitura bidirecional.
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
			if s.freecamActive {
				// Bloqueia inputs de movimento/ação enquanto freecam está ativo
				switch pk.(type) {
				case *packet.MovePlayer,
					*packet.PlayerAuthInput,
					*packet.Animate,
					*packet.InventoryTransaction,
					*packet.BlockPickRequest,
					*packet.ActorPickRequest,
					*packet.Interact,
					*packet.PlayerAction:
					continue
				}
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
			s.handleServerPacket(pk)
			if err := s.client.WritePacket(pk); err != nil {
				return
			}
		}
	}()

	<-done
	_ = s.client.Close()
	_ = s.server.Close()
}

// handleClientPacket processa pacotes do cliente. Retorna true se consumido.
func (s *session) handleClientPacket(pk packet.Packet) bool {
	switch p := pk.(type) {
	case *packet.Text:
		// TextTypeChat = 1 (constante do pacote)
		if p.TextType == packet.TextTypeChat && strings.HasPrefix(p.Message, ".") {
			s.handleCommand(p.Message)
			return true
		}
	case *packet.MovePlayer:
		if !s.freecamActive {
			s.mu.Lock()
			s.savedPosition = p.Position
			s.mu.Unlock()
		}
	case *packet.PlayerAuthInput:
		if !s.freecamActive {
			s.mu.Lock()
			s.savedPosition = p.Position
			s.mu.Unlock()
		}
	}
	return false
}

// handleServerPacket observa pacotes do servidor para manter estado.
func (s *session) handleServerPacket(pk packet.Packet) {
	switch p := pk.(type) {
	case *packet.SetPlayerGameType:
		if !s.freecamActive {
			s.mu.Lock()
			s.savedGameMode = p.GameType
			s.mu.Unlock()
		}
	}
}

// handleCommand processa um comando proxy (iniciado com ".").
func (s *session) handleCommand(raw string) {
	parts := strings.Fields(strings.ToLower(raw))
	if len(parts) == 0 {
		return
	}
	cmd := parts[0]

	switch cmd {
	case ".help", ".ajuda":
		s.sendHelp()
	case ".fullbright", ".fb":
		s.toggleFullbright()
	case ".freecam", ".fc":
		s.toggleFreecam()
	case ".stop":
		s.handleStop()
	default:
		s.sendMessage(fmt.Sprintf("§cComando desconhecido: %s — use §e.ajuda§c para ver a lista.", cmd))
	}
}

// ─────────────────────────────────────────────────────────────
//  .ajuda
// ─────────────────────────────────────────────────────────────

func (s *session) sendHelp() {
	lines := []string{
		"§b§l╔══════════════════════════════════╗",
		"§b§l║      §eDONUT PROXY §b— Comandos      §b§l║",
		"§b§l╠══════════════════════════════════╣",
		"§b§l║ §e.help §7/ §e.ajuda    §fMostra esta tela",
		"§b§l║ §e.fullbright §7/ §e.fb §fToggle visão noturna",
		"§b§l║ §e.freecam §7/ §e.fc   §fToggle câmera livre",
		"§b§l║ §e.stop          §fPara o proxy (confirme)",
		"§b§l╚══════════════════════════════════╝",
	}
	for _, l := range lines {
		s.sendMessage(l)
	}
}

// ─────────────────────────────────────────────────────────────
//  .fullbright
// ─────────────────────────────────────────────────────────────

func (s *session) toggleFullbright() {
	s.mu.Lock()
	s.fullbrightOn = !s.fullbrightOn
	on := s.fullbrightOn
	s.mu.Unlock()

	if on {
		_ = s.client.WritePacket(&packet.MobEffect{
			EntityRuntimeID: s.entityRuntimeID,
			Operation:       packet.MobEffectAdd,
			EffectType:      16, // Night Vision
			Amplifier:       0,
			Particles:       false,
			Duration:        math.MaxInt32,
		})
		s.sendMessage("§a[Fullbright] §fVisão noturna §aATIVADA§f.")
	} else {
		_ = s.client.WritePacket(&packet.MobEffect{
			EntityRuntimeID: s.entityRuntimeID,
			Operation:       packet.MobEffectRemove,
			EffectType:      16,
		})
		s.sendMessage("§c[Fullbright] §fVisão noturna §cDESATIVADA§f.")
	}
}

// ─────────────────────────────────────────────────────────────
//  .freecam
// ─────────────────────────────────────────────────────────────

func (s *session) toggleFreecam() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.freecamActive {
		s.enableFreecam()
	} else {
		s.disableFreecam()
	}
}

func (s *session) enableFreecam() {
	s.freecamActive = true
	_ = s.client.WritePacket(&packet.SetPlayerGameType{
		GameType: 6, // spectator
	})
	s.sendMessage("§a[FreeCam] §fCâmera livre §aATIVADA§f. No servidor você está parado.")
}

func (s *session) disableFreecam() {
	s.freecamActive = false
	_ = s.client.WritePacket(&packet.SetPlayerGameType{
		GameType: s.savedGameMode,
	})
	_ = s.server.WritePacket(&packet.MovePlayer{
		EntityRuntimeID: s.entityRuntimeID,
		Position:        s.savedPosition,
		Mode:            packet.MoveModeTeleport,
	})
	s.sendMessage("§c[FreeCam] §fCâmera livre §cDESATIVADA§f. Você voltou para sua posição.")
}

// ─────────────────────────────────────────────────────────────
//  .stop
// ─────────────────────────────────────────────────────────────

func (s *session) handleStop() {
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
	time.Sleep(500 * time.Millisecond)
	_ = s.client.Close()
	_ = s.server.Close()
	_ = s.listener.Close()
	log.Println("[proxy] Encerrado pelo jogador via .stop")
}

// ─────────────────────────────────────────────────────────────
//  Utilitários
// ─────────────────────────────────────────────────────────────

func (s *session) sendMessage(msg string) {
	_ = s.client.WritePacket(&packet.Text{
		TextType: packet.TextTypeSystem,
		Message:  msg,
	})
}		return fmt.Errorf("status provider: %w", err)
	}

	listener, err := minecraft.ListenConfig{
		StatusProvider: status,
	}.Listen("raknet", localAddress)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()

	fmt.Printf("[proxy] Escutando em %s → %s\n", localAddress, remoteAddress)
	fmt.Println("[proxy] Conecte seu Minecraft Bedrock no endereço: 127.0.0.1:19132")
	fmt.Println("[proxy] Digite .ajuda no chat para ver os comandos disponíveis")

	for {
		conn, err := listener.Accept()
		if err != nil {
			return fmt.Errorf("accept: %w", err)
		}
		go p.handleConn(conn.(*minecraft.Conn), listener)
	}
}

// handleConn gerencia uma conexão individual do cliente.
func (p *Proxy) handleConn(client *minecraft.Conn, listener *minecraft.Listener) {
	serverConn, err := minecraft.Dialer{
		TokenSource: p.src,
		ClientData:  client.ClientData(),
	}.Dial("raknet", remoteAddress)
	if err != nil {
		log.Printf("[proxy] Falha ao conectar no servidor: %v", err)
		_ = client.Close()
		return
	}

	// Faz o spawn dos dois lados simultaneamente.
	var wg sync.WaitGroup
	wg.Add(2)
	var spawnErr1, spawnErr2 error
	go func() {
		defer wg.Done()
		spawnErr1 = client.StartGame(serverConn.GameData())
	}()
	go func() {
		defer wg.Done()
		spawnErr2 = serverConn.DoSpawn()
	}()
	wg.Wait()

	if spawnErr1 != nil {
		log.Printf("[proxy] Erro no spawn do cliente: %v", spawnErr1)
		_ = client.Close()
		_ = serverConn.Close()
		return
	}
	if spawnErr2 != nil {
		log.Printf("[proxy] Erro no spawn do servidor: %v", spawnErr2)
		_ = client.Close()
		_ = serverConn.Close()
		return
	}

	fmt.Println("[proxy] Jogador conectado!")

	session := newSession(client, serverConn, listener)
	session.run()
}

// session representa a sessão ativa de um jogador.
type session struct {
	client     *minecraft.Conn
	server     *minecraft.Conn
	listener   *minecraft.Listener

	mu             sync.Mutex
	stopping       atomic.Bool
	stopConfirm    atomic.Bool
	stopTimer      *time.Timer

	// freecam
	freecamActive  bool
	savedGameMode  int32
	savedPosition  protocol.Vec3

	// fullbright
	fullbrightOn   bool

	// entidade do jogador (recebida no StartGame)
	entityRuntimeID uint64
}

func newSession(client *minecraft.Conn, server *minecraft.Conn, listener *minecraft.Listener) *session {
	gd := server.GameData()
	return &session{
		client:          client,
		server:          server,
		listener:        listener,
		savedGameMode:   gd.PlayerGameMode,
		entityRuntimeID: gd.EntityRuntimeID,
	}
}

// run inicia as goroutines de leitura bidirecional.
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
				// Pacote foi consumido pelo proxy, não encaminhar.
				continue
			}
			if s.freecamActive {
				// Enquanto freecam está ativo, bloqueamos inputs de movimento/ação.
				switch pk.(type) {
				case *packet.MovePlayer,
					*packet.PlayerAuthInput,
					*packet.Animate,
					*packet.InventoryTransaction,
					*packet.BlockPickRequest,
					*packet.ActorPickRequest,
					*packet.InteractBlock,
					*packet.Interact,
					*packet.PlayerAction:
					continue
				}
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
			s.handleServerPacket(pk)
			if err := s.client.WritePacket(pk); err != nil {
				return
			}
		}
	}()

	<-done
	_ = s.client.Close()
	_ = s.server.Close()
}

// handleClientPacket processa pacotes do cliente. Retorna true se o pacote foi consumido.
func (s *session) handleClientPacket(pk packet.Packet) bool {
	switch p := pk.(type) {
	case *packet.Text:
		if p.Type == packet.TextTypeChat && strings.HasPrefix(p.Message, ".") {
			s.handleCommand(p.Message)
			return true // intercepta — não manda pro servidor
		}
	case *packet.MovePlayer:
		// Atualiza posição salva para uso futuro
		if !s.freecamActive {
			s.mu.Lock()
			s.savedPosition = p.Position
			s.mu.Unlock()
		}
	case *packet.PlayerAuthInput:
		// Atualiza posição salva via PlayerAuthInput (versões mais novas)
		if !s.freecamActive {
			s.mu.Lock()
			s.savedPosition = p.Position
			s.mu.Unlock()
		}
	}
	return false
}

// handleServerPacket observa pacotes do servidor para manter estado.
func (s *session) handleServerPacket(pk packet.Packet) {
	switch p := pk.(type) {
	case *packet.SetPlayerGameType:
		if !s.freecamActive {
			s.mu.Lock()
			s.savedGameMode = p.GameType
			s.mu.Unlock()
		}
	}
}

// handleCommand processa um comando do proxy (iniciado com ".").
func (s *session) handleCommand(raw string) {
	parts := strings.Fields(strings.ToLower(raw))
	if len(parts) == 0 {
		return
	}
	cmd := parts[0]

	switch cmd {
	case ".help", ".ajuda":
		s.sendHelp()

	case ".fullbright", ".fb":
		s.toggleFullbright()

	case ".freecam", ".fc":
		s.toggleFreecam()

	case ".stop":
		s.handleStop()

	default:
		s.sendMessage(fmt.Sprintf("§cComando desconhecido: %s — use §e.ajuda§c para ver a lista.", cmd))
	}
}

// ─────────────────────────────────────────────────────────────
//  .help / .ajuda
// ─────────────────────────────────────────────────────────────

func (s *session) sendHelp() {
	lines := []string{
		"§b§l╔══════════════════════════════════╗",
		"§b§l║      §eDONUT PROXY §b— Comandos      §b§l║",
		"§b§l╠══════════════════════════════════╣",
		"§b§l║ §e.help §7/ §e.ajuda    §fMostra esta tela",
		"§b§l║ §e.fullbright §7/ §e.fb §fToggle visão noturna",
		"§b§l║ §e.freecam §7/ §e.fc   §fToggle câmera livre",
		"§b§l║ §e.stop          §fPara o proxy (confirme)",
		"§b§l╚══════════════════════════════════╝",
	}
	for _, l := range lines {
		s.sendMessage(l)
	}
}

// ─────────────────────────────────────────────────────────────
//  .fullbright / .fb
// ─────────────────────────────────────────────────────────────

func (s *session) toggleFullbright() {
	s.mu.Lock()
	s.fullbrightOn = !s.fullbrightOn
	on := s.fullbrightOn
	s.mu.Unlock()

	if on {
		// Aplica Night Vision infinito somente no client (não manda pro servidor).
		_ = s.client.WritePacket(&packet.MobEffect{
			EntityRuntimeID: s.entityRuntimeID,
			Operation:       packet.MobEffectAdd,
			EffectType:      16, // Night Vision
			Amplifier:       0,
			Particles:       false,
			Duration:        math.MaxInt32,
		})
		s.sendMessage("§a[Fullbright] §fVisão noturna §aATIVADA§f.")
	} else {
		_ = s.client.WritePacket(&packet.MobEffect{
			EntityRuntimeID: s.entityRuntimeID,
			Operation:       packet.MobEffectRemove,
			EffectType:      16,
		})
		s.sendMessage("§c[Fullbright] §fVisão noturna §cDESATIVADA§f.")
	}
}

// ─────────────────────────────────────────────────────────────
//  .freecam / .fc
// ─────────────────────────────────────────────────────────────

func (s *session) toggleFreecam() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.freecamActive {
		s.enableFreecam()
	} else {
		s.disableFreecam()
	}
}

func (s *session) enableFreecam() {
	s.freecamActive = true

	// Manda spectator para o CLIENT (só o client vê).
	_ = s.client.WritePacket(&packet.SetPlayerGameType{
		GameType: 6, // spectator
	})

	s.sendMessage("§a[FreeCam] §fCâmera livre §aATIVADA§f. No servidor você está parado.")
}

func (s *session) disableFreecam() {
	s.freecamActive = false

	// Restaura gamemode original no client.
	_ = s.client.WritePacket(&packet.SetPlayerGameType{
		GameType: s.savedGameMode,
	})

	// Teleporta o player de volta para a posição salva no servidor.
	_ = s.server.WritePacket(&packet.MovePlayer{
		EntityRuntimeID: s.entityRuntimeID,
		Position:        s.savedPosition,
		Mode:            packet.MoveModeTeleport,
	})

	s.sendMessage("§c[FreeCam] §fCâmera livre §cDESATIVADA§f. Você voltou para sua posição.")
}

// ─────────────────────────────────────────────────────────────
//  .stop
// ─────────────────────────────────────────────────────────────

func (s *session) handleStop() {
	if !s.stopping.Load() {
		// Primeira chamada: inicia contagem regressiva de 10s.
		s.stopping.Store(true)
		s.sendMessage("§e[Stop] §fDigite §c.stop§f novamente em §c10 segundos§f para confirmar a parada do proxy.")

		s.stopTimer = time.AfterFunc(10*time.Second, func() {
			s.stopping.Store(false)
			s.sendMessage("§e[Stop] §fCancelado — tempo expirado.")
		})
		return
	}

	// Segunda chamada dentro do tempo: confirma.
	if s.stopTimer != nil {
		s.stopTimer.Stop()
	}
	s.sendMessage("§c[Stop] §fEncerrando o proxy... Até mais!")
	time.Sleep(500 * time.Millisecond) // tempo para a mensagem chegar ao client
	_ = s.client.Close()
	_ = s.server.Close()
	_ = s.listener.Close()
	log.Println("[proxy] Encerrado pelo jogador via .stop")
}

// ─────────────────────────────────────────────────────────────
//  Utilitários
// ─────────────────────────────────────────────────────────────

// sendMessage envia uma mensagem de sistema visível apenas no cliente.
func (s *session) sendMessage(msg string) {
	_ = s.client.WritePacket(&packet.Text{
		Type:    packet.TextTypeSystem,
		Message: msg,
	})
}
