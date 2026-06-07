package lua

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/go-gl/mathgl/mgl32"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	lua "github.com/yuin/gopher-lua"
)

// Session é a interface que a sessão do proxy implementa para o engine Lua.
type Session interface {
	SendMessage(msg string)
	GetPosition() mgl32.Vec3
	SetPosition(pos mgl32.Vec3)
	GetYaw() float32
	GetPitch() float32
	GetGamemode() int32
	GetEntityRuntimeID() uint64
	GetServerName() string
	GetServerAddress() string
	SendPacketToClient(pk packet.Packet) error
	SendPacketToServer(pk packet.Packet) error
}

// Command representa um comando registrado por um script Lua.
type Command struct {
	Name    string
	Aliases []string
	Help    string
	Handler func(s Session, args []string)
}

// PacketHook é uma função de hook para interceptação de pacotes.
// Retorna true se o pacote deve ser bloqueado.
type PacketHook func(s Session, pk packet.Packet) bool

// Script representa um script Lua em execução.
type Script struct {
	ID       int
	Name     string // nome do arquivo
	Cancel   context.CancelFunc
	Ctx      context.Context
	commands []string        // comandos registrados por este script
	hooks    int             // número de hooks registrados
}

// ScriptInfo contém informações sobre um script para exibição.
type ScriptInfo struct {
	ID       int
	Name     string
	Commands int
	Hooks    int
}

// Engine é o motor de scripts Lua.
type Engine struct {
	mu       sync.RWMutex
	commands map[string]*Command
	aliases  map[string]string // alias -> command name

	onClientPacket []PacketHook
	onServerPacket []PacketHook

	// Sistema de scripts
	scripts    map[int]*Script
	nextID     atomic.Int32
	session    Session // sessão atual (para hooks)
}

// NewEngine cria um novo engine Lua.
func NewEngine() *Engine {
	return &Engine{
		commands: make(map[string]*Command),
		aliases:  make(map[string]string),
		scripts:  make(map[int]*Script),
	}
}

// SetSession define a sessão atual (usada para hooks).
func (e *Engine) SetSession(s Session) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.session = s
}

// GetCommand retorna um comando registrado pelo nome ou alias.
func (e *Engine) GetCommand(name string) *Command {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if cmd, ok := e.commands[name]; ok {
		return cmd
	}
	if realName, ok := e.aliases[name]; ok {
		return e.commands[realName]
	}
	return nil
}

// GetHelp retorna a lista de comandos formatada para exibição.
func (e *Engine) GetHelp() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	lines := []string{
		"§b§l╔══════════════════════════════════════╗",
		"§b§l║         §ePL Proxy §b— Comandos          §b§l║",
		"§b§l╠══════════════════════════════════════╣",
		"§b§l║ §e.ajuda §7/ §e.help      §fEsta tela",
		"§b§l║ §e.run §7<arquivo>     §fExecuta script Lua",
		"§b§l║ §e.debug §7<arquivo>   §fExecuta com erros detalhados",
		"§b§l║ §e.ids               §fLista scripts ativos",
		"§b§l║ §e.stop §7<id>        §fPara um script",
		"§b§l║ §e.stop §7             §fPara o proxy (confirme)",
	}

	if len(e.commands) > 0 {
		lines = append(lines, "§b§l╠══════════════════════════════════════╣")
		lines = append(lines, "§b§l║ §e§lComandos dos scripts:")

		for _, cmd := range e.commands {
			aliasStr := ""
			if len(cmd.Aliases) > 0 {
				aliasStr = fmt.Sprintf(" §7/ §e.%s", strings.Join(cmd.Aliases, " §7/ §e."))
			}
			lines = append(lines, fmt.Sprintf("§b§l║ §e.%s%s      §f%s", cmd.Name, aliasStr, cmd.Help))
		}
	}

	lines = append(lines, "§b§l╚══════════════════════════════════════╝")
	return lines
}

// RunFile executa um arquivo Lua em background e retorna o ID.
func (e *Engine) RunFile(path string, s Session) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("ler arquivo: %w", err)
	}

	id := int(e.nextID.Add(1))
	ctx, cancel := context.WithCancel(context.Background())

	script := &Script{
		ID:     id,
		Name:   path,
		Cancel: cancel,
		Ctx:    ctx,
	}

	// Registra o script antes de executar
	e.mu.Lock()
	e.scripts[id] = script
	e.mu.Unlock()

	// Executa em uma goroutine
	go func() {
		L := lua.NewState()
		defer L.Close()

		// Captura comandos e hooks registrados por este script
		e.registerAPIWithTracking(L, s, script)

		if err := L.DoString(string(data)); err != nil {
			fmt.Printf("[lua] Erro no script #%d (%s): %v\n", id, path, err)
			s.SendMessage(fmt.Sprintf("§c[Script] §fErro no script #%d: §c%v", id, err))
			e.StopScript(id)
			return
		}

		// Espera até ser cancelado
		<-ctx.Done()
		L.Close()
	}()

	return id, nil
}

// RunFileDebug executa um arquivo Lua com modo debug (erros detalhados).
func (e *Engine) RunFileDebug(path string, s Session) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("ler arquivo: %w", err)
	}

	id := int(e.nextID.Add(1))
	ctx, cancel := context.WithCancel(context.Background())

	script := &Script{
		ID:     id,
		Name:   path,
		Cancel: cancel,
		Ctx:    ctx,
	}

	// Registra o script antes de executar
	e.mu.Lock()
	e.scripts[id] = script
	e.mu.Unlock()

	// Executa em uma goroutine
	go func() {
		L := lua.NewState()
		defer L.Close()

		// Captura comandos e hooks registrados por este script
		e.registerAPIWithTracking(L, s, script)

		// Ativa debug
		L.SetGlobal("__debug_mode", lua.LBool(true))

		s.SendMessage(fmt.Sprintf("§e[Debug] §fExecutando script #%d: §e%s", id, path))
		s.SendMessage("§e[Debug] §fErros aparecerão no chat e no console.")

		if err := L.DoString(string(data)); err != nil {
			// Parse do erro para formato detalhado
			errMsg := err.Error()
			s.SendMessage(fmt.Sprintf("§c[Debug] §fERRO no script #%d:", id))
			s.SendMessage(fmt.Sprintf("§c  ↓ %s", errMsg))

			// Tenta extrair linha do erro
			if strings.Contains(errMsg, "[") {
				parts := strings.SplitN(errMsg, "]", 2)
				if len(parts) > 1 {
					s.SendMessage(fmt.Sprintf("§c  ↓ %s", strings.TrimSpace(parts[1])))
				}
			}

			// Log no console também
			fmt.Printf("\n§c═══════════════════════════════════════\n")
			fmt.Printf("§c  ERRO NO SCRIPT #%d (%s)\n", id, path)
			fmt.Printf("§c  %s\n", errMsg)
			fmt.Printf("§c═══════════════════════════════════════\n\n")

			e.StopScript(id)
			return
		}

		s.SendMessage(fmt.Sprintf("§a[Debug] §fScript #%d executado sem erros de sintaxe.", id))
		s.SendMessage(fmt.Sprintf("§a[Debug] §fComandos: %d | Hooks: %d", len(script.commands), script.hooks))

		// Espera até ser cancelado
		<-ctx.Done()
	}()

	return id, nil
}

// StopScript para um script pelo ID.
func (e *Engine) StopScript(id int) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	script, ok := e.scripts[id]
	if !ok {
		return false
	}

	// Cancela o contexto
	script.Cancel()

	// Remove comandos registrados por este script
	for _, cmdName := range script.commands {
		if cmd, exists := e.commands[cmdName]; exists {
			delete(e.commands, cmdName)
			for _, alias := range cmd.Aliases {
				delete(e.aliases, alias)
			}
			delete(e.commands, cmdName)
		}
	}

	// Remove hooks (precisa recriar as slices sem os hooks deste script)
	// Por simplicidade, limpa todos e deixa os scripts re-registrarem
	if script.hooks > 0 {
		e.onClientPacket = nil
		e.onServerPacket = nil
	}

	delete(e.scripts, id)
	return true
}

// GetActiveScripts retorna informações sobre todos os scripts ativos.
func (e *Engine) GetActiveScripts() []ScriptInfo {
	e.mu.RLock()
	defer e.mu.RUnlock()

	var infos []ScriptInfo
	for _, s := range e.scripts {
		infos = append(infos, ScriptInfo{
			ID:       s.ID,
			Name:     s.Name,
			Commands: len(s.commands),
			Hooks:    s.hooks,
		})
	}
	return infos
}

// IsScriptRunning verifica se um script está rodando.
func (e *Engine) IsScriptRunning(id int) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	_, ok := e.scripts[id]
	return ok
}

// ExecuteClientHooks executa todos os hooks de pacote do cliente.
func (e *Engine) ExecuteClientHooks(s Session, pk packet.Packet) bool {
	e.mu.RLock()
	hooks := make([]PacketHook, len(e.onClientPacket))
	copy(hooks, e.onClientPacket)
	e.mu.RUnlock()

	for _, hook := range hooks {
		if hook(s, pk) {
			return true
		}
	}
	return false
}

// ExecuteServerHooks executa todos os hooks de pacote do servidor.
func (e *Engine) ExecuteServerHooks(s Session, pk packet.Packet) bool {
	e.mu.RLock()
	hooks := make([]PacketHook, len(e.onServerPacket))
	copy(hooks, e.onServerPacket)
	e.mu.RUnlock()

	for _, hook := range hooks {
		if hook(s, pk) {
			return true
		}
	}
	return false
}

// Clear remove todos os comandos, hooks e scripts.
func (e *Engine) Clear() {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Cancela todos os scripts
	for _, s := range e.scripts {
		s.Cancel()
	}

	e.commands = make(map[string]*Command)
	e.aliases = make(map[string]string)
	e.onClientPacket = nil
	e.onServerPacket = nil
	e.scripts = make(map[int]*Script)
}

func (e *Engine) registerAPIWithTracking(L *lua.LState, s Session, script *Script) {
	proxy := L.NewTable()

	// ── Mensagens ──────────────────────────────────────────
	L.SetField(proxy, "send_message", L.NewFunction(func(L *lua.LState) int {
		msg := L.CheckString(1)
		s.SendMessage(msg)
		return 0
	}))

	// ── Posição ────────────────────────────────────────────
	L.SetField(proxy, "get_position", L.NewFunction(func(L *lua.LState) int {
		pos := s.GetPosition()
		tbl := L.NewTable()
		tbl.RawSetString("x", lua.LNumber(pos.X()))
		tbl.RawSetString("y", lua.LNumber(pos.Y()))
		tbl.RawSetString("z", lua.LNumber(pos.Z()))
		L.Push(tbl)
		return 1
	}))

	L.SetField(proxy, "set_position", L.NewFunction(func(L *lua.LState) int {
		x := float32(L.CheckNumber(1))
		y := float32(L.CheckNumber(2))
		z := float32(L.CheckNumber(3))
		s.SetPosition(mgl32.Vec3{x, y, z})
		return 0
	}))

	L.SetField(proxy, "get_yaw", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LNumber(s.GetYaw()))
		return 1
	}))

	L.SetField(proxy, "get_pitch", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LNumber(s.GetPitch()))
		return 1
	}))

	// ── Gamemode ───────────────────────────────────────────
	L.SetField(proxy, "get_gamemode", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LNumber(s.GetGamemode()))
		return 1
	}))

	// ── Info ───────────────────────────────────────────────
	L.SetField(proxy, "get_entity_runtime_id", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LNumber(s.GetEntityRuntimeID()))
		return 1
	}))

	L.SetField(proxy, "get_server_name", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LString(s.GetServerName()))
		return 1
	}))

	L.SetField(proxy, "get_server_address", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LString(s.GetServerAddress()))
		return 1
	}))

	L.SetField(proxy, "get_script_id", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LNumber(script.ID))
		return 1
	}))

	// ── Pacotes ────────────────────────────────────────────
	L.SetField(proxy, "send_packet_to_client", L.NewFunction(func(L *lua.LState) int {
		pkName := L.CheckString(1)
		pk := buildPacket(pkName, L.OptTable(2, nil))
		if pk == nil {
			L.Push(lua.LFalse)
			return 1
		}
		err := s.SendPacketToClient(pk)
		L.Push(lua.LBool(err == nil))
		return 1
	}))

	L.SetField(proxy, "send_packet_to_server", L.NewFunction(func(L *lua.LState) int {
		pkName := L.CheckString(1)
		pk := buildPacket(pkName, L.OptTable(2, nil))
		if pk == nil {
			L.Push(lua.LFalse)
			return 1
		}
		err := s.SendPacketToServer(pk)
		L.Push(lua.LBool(err == nil))
		return 1
	}))

	// ── Registro de comandos ───────────────────────────────
	L.SetField(proxy, "register_command", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		help := L.OptString(2, "")
		handler := L.CheckFunction(3)

		var aliases []string
		if tbl := L.OptTable(4, nil); tbl != nil {
			tbl.ForEach(func(_ lua.LValue, v lua.LValue) {
				if s, ok := v.(lua.LString); ok {
					aliases = append(aliases, string(s))
				}
			})
		}

		cmd := &Command{
			Name:    name,
			Aliases: aliases,
			Help:    help,
			Handler: func(s Session, args []string) {
				luaArgs := L.NewTable()
				for i, arg := range args {
					luaArgs.RawSetInt(i+1, lua.LString(arg))
				}
				if err := L.CallByParam(lua.P{
					Fn:      handler,
					NRet:    0,
					Protect: true,
				}, luaArgs); err != nil {
					fmt.Printf("[lua] Erro no comando .%s (script #%d): %v\n", name, script.ID, err)
				}
			},
		}

		e.mu.Lock()
		e.commands[name] = cmd
		for _, alias := range aliases {
			e.aliases[alias] = name
		}
		script.commands = append(script.commands, name)
		e.mu.Unlock()

		return 0
	}))

	// ── Hooks de pacotes ───────────────────────────────────
	L.SetField(proxy, "on_client_packet", L.NewFunction(func(L *lua.LState) int {
		handler := L.CheckFunction(1)

		hook := func(s Session, pk packet.Packet) bool {
			pkName := packetName(pk)
			tbl := packetToTable(pk, pkName)

			var blocked bool
			if err := L.CallByParam(lua.P{
				Fn:      handler,
				NRet:    1,
				Protect: true,
			}, lua.LString(pkName), tbl); err != nil {
				fmt.Printf("[lua] Erro no hook on_client_packet (script #%d): %v\n", script.ID, err)
				return false
			}

			ret := L.Get(-1)
			L.Pop(1)
			if b, ok := ret.(lua.LBool); ok {
				blocked = bool(b)
			}
			return blocked
		}

		e.mu.Lock()
		e.onClientPacket = append(e.onClientPacket, hook)
		script.hooks++
		e.mu.Unlock()

		return 0
	}))

	L.SetField(proxy, "on_server_packet", L.NewFunction(func(L *lua.LState) int {
		handler := L.CheckFunction(1)

		hook := func(s Session, pk packet.Packet) bool {
			pkName := packetName(pk)
			tbl := packetToTable(pk, pkName)

			var blocked bool
			if err := L.CallByParam(lua.P{
				Fn:      handler,
				NRet:    1,
				Protect: true,
			}, lua.LString(pkName), tbl); err != nil {
				fmt.Printf("[lua] Erro no hook on_server_packet (script #%d): %v\n", script.ID, err)
				return false
			}

			ret := L.Get(-1)
			L.Pop(1)
			if b, ok := ret.(lua.LBool); ok {
				blocked = bool(b)
			}
			return blocked
		}

		e.mu.Lock()
		e.onServerPacket = append(e.onServerPacket, hook)
		script.hooks++
		e.mu.Unlock()

		return 0
	}))

	// ── Log ────────────────────────────────────────────────
	L.SetField(proxy, "log", L.NewFunction(func(L *lua.LState) int {
		msg := L.CheckString(1)
		fmt.Printf("[lua] #%d: %s\n", script.ID, msg)
		return 0
	}))

	// ── Constantes de gamemode ─────────────────────────────
	L.SetField(proxy, "GAMETYPE_SURVIVAL", lua.LNumber(0))
	L.SetField(proxy, "GAMETYPE_CREATIVE", lua.LNumber(1))
	L.SetField(proxy, "GAMETYPE_ADVENTURE", lua.LNumber(2))
	L.SetField(proxy, "GAMETYPE_SPECTATOR", lua.LNumber(6))

	// ── Constantes de operação de efeito ───────────────────
	L.SetField(proxy, "EFFECT_ADD", lua.LNumber(1))
	L.SetField(proxy, "EFFECT_MODIFY", lua.LNumber(2))
	L.SetField(proxy, "EFFECT_REMOVE", lua.LNumber(3))

	// ── Constantes de TitleAction ──────────────────────────
	L.SetField(proxy, "TITLE_CLEAR", lua.LNumber(0))
	L.SetField(proxy, "TITLE_RESET", lua.LNumber(1))
	L.SetField(proxy, "TITLE_SET", lua.LNumber(2))
	L.SetField(proxy, "TITLE_SET_SUBTITLE", lua.LNumber(3))
	L.SetField(proxy, "TITLE_SET_ACTIONBAR", lua.LNumber(4))
	L.SetField(proxy, "TITLE_SET_DURATIONS", lua.LNumber(5))

	// ── Constantes de MoveMode ─────────────────────────────
	L.SetField(proxy, "MOVE_NORMAL", lua.LNumber(0))
	L.SetField(proxy, "MOVE_RESET", lua.LNumber(1))
	L.SetField(proxy, "MOVE_TELEPORT", lua.LNumber(2))
	L.SetField(proxy, "MOVE_ROTATION", lua.LNumber(3))

	L.SetGlobal("proxy", proxy)
}

// buildPacket cria um pacote a partir do nome e de uma tabela Lua.
func buildPacket(name string, data *lua.LTable) packet.Packet {
	switch name {
	case "Text":
		pk := &packet.Text{TextType: packet.TextTypeChat}
		if data != nil {
			pk.Message = getStringField(data, "message", "")
			pk.TextType = byte(getNumberField(data, "type", float64(packet.TextTypeChat)))
			pk.SourceName = getStringField(data, "source_name", "")
			pk.XUID = getStringField(data, "xuid", "")
			pk.PlatformChatID = getStringField(data, "platform_chat_id", "")
		}
		return pk

	case "MovePlayer":
		pk := &packet.MovePlayer{Mode: byte(packet.MoveModeTeleport)}
		if data != nil {
			pk.EntityRuntimeID = uint64(getNumberField(data, "entity_runtime_id", 0))
			pk.Position = mgl32.Vec3{
				float32(getNumberField(data, "x", 0)),
				float32(getNumberField(data, "y", 0)),
				float32(getNumberField(data, "z", 0)),
			}
			pk.HeadYaw = float32(getNumberField(data, "head_yaw", 0))
			pk.Pitch = float32(getNumberField(data, "pitch", 0))
			pk.Yaw = float32(getNumberField(data, "yaw", 0))
			pk.Mode = byte(getNumberField(data, "mode", float64(packet.MoveModeTeleport)))
		}
		return pk

	case "MobEffect":
		pk := &packet.MobEffect{}
		if data != nil {
			pk.EntityRuntimeID = uint64(getNumberField(data, "entity_runtime_id", 0))
			pk.Operation = byte(getNumberField(data, "operation", float64(packet.MobEffectAdd)))
			pk.EffectType = int32(getNumberField(data, "effect_type", 0))
			pk.Amplifier = int32(getNumberField(data, "amplifier", 0))
			pk.Particles = getBoolField(data, "particles", false)
			pk.Duration = int32(getNumberField(data, "duration", 0))
		}
		return pk

	case "SetTitle":
		pk := &packet.SetTitle{}
		if data != nil {
			pk.ActionType = int32(getNumberField(data, "action", 0))
			pk.Text = getStringField(data, "text", "")
			pk.FadeInDuration = int32(getNumberField(data, "fade_in", 0))
			pk.RemainDuration = int32(getNumberField(data, "stay", 0))
			pk.FadeOutDuration = int32(getNumberField(data, "fade_out", 0))
		}
		return pk

	case "NetworkStackLatency":
		pk := &packet.NetworkStackLatency{NeedsResponse: true}
		if data != nil {
			pk.Timestamp = int64(getNumberField(data, "timestamp", 0))
			pk.NeedsResponse = getBoolField(data, "needs_response", true)
		}
		return pk

	case "PlaySound":
		pk := &packet.PlaySound{}
		if data != nil {
			pk.SoundName = getStringField(data, "sound_name", "")
			pk.Position = mgl32.Vec3{
				float32(getNumberField(data, "x", 0)),
				float32(getNumberField(data, "y", 0)),
				float32(getNumberField(data, "z", 0)),
			}
			pk.Volume = float32(getNumberField(data, "volume", 1))
			pk.Pitch = float32(getNumberField(data, "pitch", 1))
		}
		return pk
	}

	return nil
}

// packetName retorna o nome simplificado de um pacote.
func packetName(pk packet.Packet) string {
	t := fmt.Sprintf("%T", pk)
	parts := strings.Split(t, ".")
	return parts[len(parts)-1]
}

// packetToTable converte um pacote para uma tabela Lua.
func packetToTable(pk packet.Packet, name string) *lua.LTable {
	tbl := &lua.LTable{}

	switch p := pk.(type) {
	case *packet.Text:
		tbl.RawSetString("message", lua.LString(p.Message))
		tbl.RawSetString("type", lua.LNumber(p.TextType))
		tbl.RawSetString("source_name", lua.LString(p.SourceName))

	case *packet.MovePlayer:
		tbl.RawSetString("entity_runtime_id", lua.LNumber(p.EntityRuntimeID))
		tbl.RawSetString("x", lua.LNumber(p.Position.X()))
		tbl.RawSetString("y", lua.LNumber(p.Position.Y()))
		tbl.RawSetString("z", lua.LNumber(p.Position.Z()))
		tbl.RawSetString("head_yaw", lua.LNumber(p.HeadYaw))
		tbl.RawSetString("pitch", lua.LNumber(p.Pitch))
		tbl.RawSetString("yaw", lua.LNumber(p.Yaw))
		tbl.RawSetString("mode", lua.LNumber(p.Mode))

	case *packet.SetPlayerGameType:
		tbl.RawSetString("gametype", lua.LNumber(p.GameType))

	case *packet.MobEffect:
		tbl.RawSetString("entity_runtime_id", lua.LNumber(p.EntityRuntimeID))
		tbl.RawSetString("effect_type", lua.LNumber(p.EffectType))
		tbl.RawSetString("duration", lua.LNumber(p.Duration))

	case *packet.NetworkStackLatency:
		tbl.RawSetString("timestamp", lua.LNumber(p.Timestamp))
		tbl.RawSetString("needs_response", lua.LBool(p.NeedsResponse))

	case *packet.PlaySound:
		tbl.RawSetString("sound_name", lua.LString(p.SoundName))
		tbl.RawSetString("x", lua.LNumber(p.Position.X()))
		tbl.RawSetString("y", lua.LNumber(p.Position.Y()))
		tbl.RawSetString("z", lua.LNumber(p.Position.Z()))
	}

	return tbl
}

func getStringField(tbl *lua.LTable, key, def string) string {
	if v := tbl.RawGetString(key); v != lua.LNil {
		return v.String()
	}
	return def
}

func getNumberField(tbl *lua.LTable, key string, def float64) float64 {
	if v := tbl.RawGetString(key); v != lua.LNil {
		if n, ok := v.(lua.LNumber); ok {
			return float64(n)
		}
	}
	return def
}

func getBoolField(tbl *lua.LTable, key string, def bool) bool {
	if v := tbl.RawGetString(key); v != lua.LNil {
		if b, ok := v.(lua.LBool); ok {
			return bool(b)
		}
	}
	return def
}
