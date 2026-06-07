package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/sandertv/gophertunnel/minecraft/auth"
	"github.com/user/donut-proxy/proxy"
	"golang.org/x/oauth2"
)

const tokenFile = "token.json"

func main() {
	fmt.Println(`
 ██████╗  ██████╗ ███╗   ██╗██╗   ██╗████████╗    ██████╗ ██████╗  ██████╗ ██╗  ██╗██╗   ██╗
 ██╔══██╗██╔═══██╗████╗  ██║██║   ██║╚══██╔══╝    ██╔══██╗██╔══██╗██╔═══██╗╚██╗██╔╝╚██╗ ██╔╝
 ██║  ██║██║   ██║██╔██╗ ██║██║   ██║   ██║       ██████╔╝██████╔╝██║   ██║ ╚███╔╝  ╚████╔╝ 
 ██║  ██║██║   ██║██║╚██╗██║██║   ██║   ██║       ██╔═══╝ ██╔══██╗██║   ██║ ██╔██╗   ╚██╔╝  
 ██████╔╝╚██████╔╝██║ ╚████║╚██████╔╝   ██║       ██║     ██║  ██║╚██████╔╝██╔╝ ██╗   ██║   
 ╚═════╝  ╚═════╝ ╚═╝  ╚═══╝ ╚═════╝    ╚═╝       ╚═╝     ╚═╝  ╚═╝ ╚═════╝ ╚═╝  ╚═╝   ╚═╝  
                          para donutsmp.net — by você mesmo
`)

	token, err := loadToken()
	if err != nil {
		fmt.Println("[auth] Nenhum token salvo encontrado. Fazendo login com conta Microsoft...")
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
		fmt.Println("[auth] Token carregado de", tokenFile)
	}

	src := auth.RefreshTokenSource(token)

	// Ao renovar o token, persiste a versão atualizada
	go watchAndPersistToken(src)

	p := proxy.New(src)
	if err := p.Start(); err != nil {
		log.Fatalf("[proxy] Erro fatal: %v", err)
	}
}

// loadToken tenta carregar o token OAuth2 do disco.
func loadToken() (*oauth2.Token, error) {
	data, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, err
	}
	var t oauth2.Token
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// saveToken persiste o token OAuth2 em disco.
func saveToken(t *oauth2.Token) error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(tokenFile, data, 0600)
}

// watchAndPersistToken observa renovações de token e as salva.
func watchAndPersistToken(src oauth2.TokenSource) {
	// A cada vez que o token for acessado e renovado, salvamos.
	// Como gophertunnel usa o src internamente, salvamos o estado inicial.
	t, err := src.Token()
	if err != nil {
		return
	}
	_ = saveToken(t)
}
