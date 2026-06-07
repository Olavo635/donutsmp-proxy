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

type Proxy struct {
	src oauth2.TokenSource
}

func New(src oauth2.TokenSource) *Proxy {
	return &Proxy{src: src}
}

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
	newSession(client, serverConn, listener).run()
}

// ─────────────────────────────────────────────────────────────
//  session
// ─────────────────────────────────────────────────────────────

type session struct {
	client   *minecraft.Conn
	server   *minecraft.Conn
	listener *minecraft.Listener

	mu        sync.Mutex
	stopping  atomic.Bool
	stopTimer *time.Timer

	// freecam
	freecamActive bool
	savedGameMode int32
	savedPosition mgl32.Vec3

	// fullbright
	fullbrightOn bool

	// nochat
	nochatOn bool

	// runtime ID do jogador
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
			if s.handleServerPacket(pk) {
				// pacote bloqueado pelo proxy, não encaminha
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

// handleClientPacket processa pacotes do cliente. Retorna true se consumido.
func (s *session) handleClientPacket(pk packet.Packet) bool {
	switch p := pk.(type) {
	case *packet.Text:
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

// handleServerPacket processa pacotes do servidor. Retorna true se o pacote deve ser bloqueado.
func (s *session) handleServerPacket(pk packet.Packet) bool {
	switch p := pk.(type) {
	case *packet.SetPlayerGameType:
		if !s.freecamActive {
			s.mu.Lock()
			s.savedGameMode = p.GameType
			s.mu.Unlock()
		}
	case *packet.Text:
		s.mu.Lock()
		nochat := s.nochatOn
		s.mu.Unlock()
		if nochat && p.TextType != packet.TextTypeSystem {
			return true // bloqueia chat, whispers, anuncios, etc
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────
//  Comandos
// ─────────────────────────────────────────────────────────────

func (s *session) handleCommand(raw string) {
	parts := strings.Fields(strings.ToLower(raw))
	if len(parts) == 0 {
		return
	}
	switch parts[0] {
	case ".help", ".ajuda":
		s.sendHelp()
	case ".fullbright", ".fb":
		s.toggleFullbright()
	case ".freecam", ".fc":
		s.toggleFreecam()
	case ".nochat", ".nc":
		s.toggleNochat()
	case ".stop":
		s.handleStop()
	default:
		s.sendMessage(fmt.Sprintf("§cComando desconhecido: %s — use §e.ajuda§c para ver a lista.", parts[0]))
	}
}

func (s *session) sendHelp() {
	lines := []string{
		"§b§l╔════════════════════════════════════╗",
		"§b§l║       §eDONUT PROXY §b— Comandos       §b§l║",
		"§b§l╠════════════════════════════════════╣",
		"§b§l║ §e.help §7/ §e.ajuda     §fMostra esta tela",
		"§b§l║ §e.fullbright §7/ §e.fb  §fToggle visão noturna",
		"§b§l║ §e.freecam §7/ §e.fc    §fToggle câmera livre",
		"§b§l║ §e.nochat §7/ §e.nc     §fToggle silenciar chat",
		"§b§l║ §e.stop           §fPara o proxy (confirme)",
		"§b§l╚════════════════════════════════════╝",
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
	_ = s.client.WritePacket(&packet.SetPlayerGameType{GameType: 6})
	s.sendMessage("§a[FreeCam] §fCâmera livre §aATIVADA§f. No servidor você está parado.")
}

func (s *session) disableFreecam() {
	s.freecamActive = false
	_ = s.client.WritePacket(&packet.SetPlayerGameType{GameType: s.savedGameMode})
	_ = s.server.WritePacket(&packet.MovePlayer{
		EntityRuntimeID: s.entityRuntimeID,
		Position:        s.savedPosition,
		Mode:            packet.MoveModeTeleport,
	})
	s.sendMessage("§c[FreeCam] §fCâmera livre §cDESATIVADA§f. Você voltou para sua posição.")
}

// ─────────────────────────────────────────────────────────────
//  .nochat
// ─────────────────────────────────────────────────────────────

func (s *session) toggleNochat() {
	s.mu.Lock()
	s.nochatOn = !s.nochatOn
	on := s.nochatOn
	s.mu.Unlock()

	if on {
		s.sendMessage("§a[NoChat] §fChat do servidor §aBLOQUEADO§f. Só mensagens do proxy aparecem.")
	} else {
		s.sendMessage("§c[NoChat] §fChat do servidor §cLIBERADO§f.")
	}
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
//  Util
// ─────────────────────────────────────────────────────────────

func (s *session) sendMessage(msg string) {
	_ = s.client.WritePacket(&packet.Text{
		TextType: packet.TextTypeSystem,
		Message:  msg,
	})
}
