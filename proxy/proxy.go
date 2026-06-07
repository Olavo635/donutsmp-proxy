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
}

func New(src oauth2.TokenSource, server Server) *Proxy {
	return &Proxy{src: src, server: server}
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
	newSession(client, serverConn, listener, p.server).run()
}

// ─────────────────────────────────────────────────────────────
//  session
// ─────────────────────────────────────────────────────────────

type session struct {
	client   *minecraft.Conn
	server   *minecraft.Conn
	listener *minecraft.Listener
	srvInfo  Server

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

	// toggles
	freecamActive bool
	fullbrightOn  bool
	nochatOn      bool
}

func newSession(client, server *minecraft.Conn, listener *minecraft.Listener, srv Server) *session {
	gd := server.GameData()
	return &session{
		client:          client,
		server:          server,
		listener:        listener,
		srvInfo:         srv,
		connectedAt:     time.Now(),
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
		if !s.freecamActive {
			s.mu.Lock()
			s.savedPosition = p.Position
			s.savedYaw = p.HeadYaw
			s.savedPitch = p.Pitch
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

// handleServerPacket — retorna true se o pacote deve ser bloqueado.
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
			return true
		}
	case *packet.SetTitle:
		s.mu.Lock()
		nochat := s.nochatOn
		s.mu.Unlock()
		if nochat {
			return true
		}
	case *packet.BossEvent:
		s.mu.Lock()
		nochat := s.nochatOn
		s.mu.Unlock()
		if nochat {
			return true
		}
	case *packet.PlaySound:
		s.mu.Lock()
		nochat := s.nochatOn
		s.mu.Unlock()
		if nochat && strings.Contains(p.SoundName, "note") {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────
//  Roteador de comandos
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
	case ".coords", ".co":
		s.showCoords()
	case ".ping":
		s.showPing()
	case ".time", ".hora":
		s.showTime()
	case ".uptime", ".up":
		s.showUptime()
	case ".clip", ".cl":
		s.doClip(parts)
	case ".server", ".srv":
		s.showServer()
	case ".stop":
		s.handleStop()
	default:
		s.sendMessage(fmt.Sprintf("§cComando desconhecido: §e%s §c— use §e.ajuda§c para ver a lista.", parts[0]))
	}
}

// ─────────────────────────────────────────────────────────────
//  .ajuda
// ─────────────────────────────────────────────────────────────

func (s *session) sendHelp() {
	lines := []string{
		"§b§l╔══════════════════════════════════════╗",
		"§b§l║         §ePL Proxy §b— Comandos          §b§l║",
		"§b§l╠══════════════════════════════════════╣",
		"§b§l║ §e.ajuda §7/ §e.help      §fEsta tela",
		"§b§l║ §e.fullbright §7/ §e.fb   §fToggle visão noturna",
		"§b§l║ §e.freecam §7/ §e.fc     §fToggle câmera livre",
		"§b§l║ §e.nochat §7/ §e.nc      §fToggle silenciar chat",
		"§b§l║ §e.coords §7/ §e.co      §fMostra suas coordenadas",
		"§b§l║ §e.ping           §fMostra o ping atual",
		"§b§l║ §e.time §7/ §e.hora      §fHora atual do sistema",
		"§b§l║ §e.uptime §7/ §e.up      §fTempo conectado",
		"§b§l║ §e.clip [n] §7/ §e.cl    §fTeleporta N blocos acima",
		"§b§l║ §e.server §7/ §e.srv     §fServidor atual",
		"§b§l║ §e.stop           §fPara o proxy (confirme)",
		"§b§l╚══════════════════════════════════════╝",
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
		s.freecamActive = true
		_ = s.client.WritePacket(&packet.SetPlayerGameType{GameType: 6})
		s.sendMessage("§a[FreeCam] §fCâmera livre §aATIVADA§f. No servidor você está parado.")
	} else {
		s.freecamActive = false
		_ = s.client.WritePacket(&packet.SetPlayerGameType{GameType: s.savedGameMode})
		_ = s.server.WritePacket(&packet.MovePlayer{
			EntityRuntimeID: s.entityRuntimeID,
			Position:        s.savedPosition,
			HeadYaw:         s.savedYaw,
			Pitch:           s.savedPitch,
			Mode:            packet.MoveModeTeleport,
		})
		s.sendMessage("§c[FreeCam] §fCâmera livre §cDESATIVADA§f. Você voltou para sua posição.")
	}
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
		_ = s.client.WritePacket(&packet.SetTitle{ActionType: packet.TitleActionClear})
		_ = s.client.WritePacket(&packet.SetTitle{ActionType: packet.TitleActionReset})
		s.sendMessage("§a[NoChat] §fChat §aBLOQUEADO§f. Só mensagens do proxy aparecem.")
	} else {
		s.sendMessage("§c[NoChat] §fChat §cLIBERADO§f.")
	}
}

// ─────────────────────────────────────────────────────────────
//  .coords
// ─────────────────────────────────────────────────────────────

func (s *session) showCoords() {
	s.mu.Lock()
	pos := s.savedPosition
	s.mu.Unlock()
	s.sendMessage(fmt.Sprintf(
		"§b[Coords] §fX: §e%.1f §fY: §e%.1f §fZ: §e%.1f",
		pos.X(), pos.Y(), pos.Z(),
	))
}

// ─────────────────────────────────────────────────────────────
//  .ping
// ─────────────────────────────────────────────────────────────

func (s *session) showPing() {
	// Mede RTT simples via tempo de WritePacket + ReadPacket
	start := time.Now()
	// Envia um NetworkStackLatency pro servidor e mede o tempo de resposta
	_ = s.server.WritePacket(&packet.NetworkStackLatency{
		Timestamp:     uint64(start.UnixMilli()),
		NeedsResponse: true,
	})
	elapsed := time.Since(start)
	s.sendMessage(fmt.Sprintf("§b[Ping] §f%d ms §7(estimado)", elapsed.Milliseconds()))
}

// ─────────────────────────────────────────────────────────────
//  .time
// ─────────────────────────────────────────────────────────────

func (s *session) showTime() {
	now := time.Now()
	s.sendMessage(fmt.Sprintf(
		"§b[Hora] §f%s §7(%s)",
		now.Format("15:04:05"),
		now.Format("02/01/2006"),
	))
}

// ─────────────────────────────────────────────────────────────
//  .uptime
// ─────────────────────────────────────────────────────────────

func (s *session) showUptime() {
	d := time.Since(s.connectedAt).Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	sec := int(d.Seconds()) % 60
	s.sendMessage(fmt.Sprintf("§b[Uptime] §fConectado há §e%02d:%02d:%02d§f.", h, m, sec))
}

// ─────────────────────────────────────────────────────────────
//  .clip [n]
// ─────────────────────────────────────────────────────────────

func (s *session) doClip(parts []string) {
	blocks := float32(3.0) // padrão: 3 blocos acima
	if len(parts) >= 2 {
		var v float64
		if _, err := fmt.Sscanf(parts[1], "%f", &v); err == nil {
			blocks = float32(v)
		}
	}
	if blocks > 256 {
		s.sendMessage("§c[Clip] §fMáximo de 256 blocos por vez.")
		return
	}

	s.mu.Lock()
	pos := s.savedPosition
	s.mu.Unlock()

	newPos := mgl32.Vec3{pos.X(), pos.Y() + blocks, pos.Z()}

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

	s.mu.Lock()
	s.savedPosition = newPos
	s.mu.Unlock()

	s.sendMessage(fmt.Sprintf("§a[Clip] §fTeleportado §e%.0f§f blocos acima. Novo Y: §e%.1f", blocks, newPos.Y()))
}

// ─────────────────────────────────────────────────────────────
//  .server
// ─────────────────────────────────────────────────────────────

func (s *session) showServer() {
	s.sendMessage(fmt.Sprintf("§b[Server] §f%s §7— §f%s", s.srvInfo.Name, s.srvInfo.Address))
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
