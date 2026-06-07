package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/sandertv/gophertunnel/minecraft/auth"
	"github.com/Olavo635/plproxy/lua"
	"github.com/Olavo635/plproxy/proxy"
	"golang.org/x/oauth2"
)

const tokenFile = "token.json"

// Servidores predefinidos
var presetServers = []proxy.Server{
	{Name: "DonutSMP", Address: "donutsmp.net:19132"},
	{Name: "Hypixel (Bedrock)", Address: "bedrock.hypixel.net:19132"},
	{Name: "CubeCraft", Address: "play.cubecraft.net:19132"},
	{Name: "Mineplex", Address: "pe.mineplex.com:19132"},
	{Name: "Hive", Address: "geo.hivebedrock.network:19132"},
	{Name: "FallenTech", Address: "fallentech.com.br:19132"},
}

func main() {
	fmt.Print("\033[H\033[2J") // limpa terminal
	fmt.Println(`
 ██████╗ ██╗         ██████╗ ██████╗  ██████╗ ██╗  ██╗██╗   ██╗
 ██╔══██╗██║         ██╔══██╗██╔══██╗██╔═══██╗╚██╗██╔╝╚██╗ ██╔╝
 ██████╔╝██║         ██████╔╝██████╔╝██║   ██║ ╚███╔╝  ╚████╔╝ 
 ██╔═══╝ ██║         ██╔═══╝ ██╔══██╗██║   ██║ ██╔██╗   ╚██╔╝  
 ██║     ███████╗    ██║     ██║  ██║╚██████╔╝██╔╝ ██╗   ██║   
 ╚═╝     ╚══════╝    ╚═╝     ╚═╝  ╚═╝ ╚═════╝ ╚═╝  ╚═╝   ╚═╝  
                    PL Proxy — Bedrock Edition
                    Lua Script Engine
`)

	// ── Auth ──────────────────────────────────────────────────
	token, err := loadToken()
	if err != nil {
		fmt.Println("[auth] Nenhum token encontrado. Fazendo login com conta Microsoft...")
		token, err = auth.RequestLiveToken()
		if err != nil {
			log.Fatalf("[auth] Falha no login: %v", err)
		}
		if saveErr := saveToken(token); saveErr != nil {
			log.Printf("[auth] Aviso: não foi possível salvar o token: %v", saveErr)
		} else {
			fmt.Println("[auth] Token salvo em", tokenFile)
		}
	} else {
		fmt.Println("[auth] Login automático via token salvo.")
	}

	src := auth.RefreshTokenSource(token)
	go persistToken(src)

	// ── Engine Lua ────────────────────────────────────────────
	engine := lua.NewEngine()
	fmt.Println("[lua] Engine Lua inicializado")

	// ── Seleção de servidor ───────────────────────────────────
	server := selectServer()

	fmt.Printf("\n[proxy] Conectando em: %s (%s)\n", server.Name, server.Address)

	p := proxy.New(src, server, engine)
	if err := p.Start(); err != nil {
		log.Fatalf("[proxy] Erro fatal: %v", err)
	}
}

func selectServer() proxy.Server {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("\n┌─────────────────────────────────┐")
	fmt.Println("│       Selecione o servidor       │")
	fmt.Println("├─────────────────────────────────┤")
	for i, s := range presetServers {
		fmt.Printf("│  [%d] %-28s│\n", i+1, s.Name)
	}
	fmt.Printf("│  [%d] %-28s│\n", len(presetServers)+1, "Servidor customizado")
	fmt.Println("└─────────────────────────────────┘")
	fmt.Print("\nOpção: ")

	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	n, err := strconv.Atoi(line)

	if err != nil || n < 1 || n > len(presetServers)+1 {
		fmt.Println("[!] Opção inválida, usando DonutSMP.")
		return presetServers[0]
	}

	if n == len(presetServers)+1 {
		fmt.Print("Endereço (ex: play.meuserver.com:19132): ")
		addr, _ := reader.ReadString('\n')
		addr = strings.TrimSpace(addr)
		if addr == "" {
			fmt.Println("[!] Endereço vazio, usando DonutSMP.")
			return presetServers[0]
		}
		// Adiciona porta padrão se não informada
		if !strings.Contains(addr, ":") {
			addr += ":19132"
		}
		return proxy.Server{Name: "Custom", Address: addr}
	}

	return presetServers[n-1]
}

func loadToken() (*oauth2.Token, error) {
	data, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, err
	}
	var t oauth2.Token
	return &t, json.Unmarshal(data, &t)
}

func saveToken(t *oauth2.Token) error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(tokenFile, data, 0600)
}

func persistToken(src oauth2.TokenSource) {
	t, err := src.Token()
	if err != nil {
		return
	}
	_ = saveToken(t)
}
