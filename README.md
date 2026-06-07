# 🚀 Pl Proxy

Proxy MITM para Minecraft Bedrock Edition com funções QOL

## Funcionalidades

| Comando | Alias | Descrição |
|---|---|---|
| `.ajuda` | `.help` | Lista todos os comandos |
| `.fullbright` | `.fb` | Toggle de visão noturna (só no seu client) |
| `.freecam` | `.fc` | Toggle câmera livre (você fica parado no server) |
| `.stop` | — | Para o proxy (requer confirmação em 10s) |
| `.coords` | `.co` | Mostra X, Y, Z atual |
| `.ping` | — | Ping estimado com o servidor |
| `.time` | `.hora` | Hora e data do sistema |
| `.uptime` | `.up` | Tempo conectado na sessão |
| `.clip [n]` | `.cl` | Teleporta N blocos acima (padrão 3, máx 256) |
| `.server` | `.srv` | Mostra servidor atual e endereço |

### Como o FreeCam funciona
- Ao ativar: o proxy muda seu gamemode para **Spectator** localmente. No servidor, você fica **parado no lugar** (inputs de movimento bloqueados).  
- Ao desativar: gamemode original é restaurado e o servidor recebe um teleport de volta para sua posição salva.

### Fullbright
Aplica o efeito Night Vision **somente no seu client**. O servidor não vê nada, sem risco de ban por efeito suspeito.

---

## Requisitos

- Go 1.24+
- Conta Microsoft/Xbox vinculada ao Minecraft Bedrock

## Instalação

```bash
git clone https://github.com/Olavo635/donutsmp-proxy
cd donutsmp-proxy
go mod tidy
go build -o donut-proxy .
```

## Uso

1. **Execute o proxy:**
   ```bash
   cd ~/donutsmp-proxy
   ./donut-proxy
   ```
2. Na **primeira execução**, ele abrirá um link no terminal para autenticação Microsoft. Faça login e copie o código.
3. O token é salvo em `token.json` — nas próximas execuções o login é automático.
4. No Minecraft Bedrock, adicione um servidor:
   - **Endereço:** `127.0.0.1`
   - **Porta:** `19132`
5. Entre no servidor e use `.ajuda` no chat.

## Estrutura do Projeto

```
donut-proxy/
├── main.go          # Entrypoint, autenticação, persistência de token
├── proxy/
│   └── proxy.go     # Listener, session, interceptação de pacotes e comandos
├── go.mod
└── README.md
```

## Notas

- O proxy usa **autenticação online** (Microsoft Live) — sua conta real, sem modo offline.
- O token OAuth2 é salvo localmente e renovado automaticamente.
- Nenhum pacote é injetado no servidor sem sua ação — apenas o FreeCam envia um teleport ao desativar.
