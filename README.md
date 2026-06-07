# Pl Proxy

Proxy MITM para Minecraft Bedrock Edition com funções QOL (Quality of Life).

## Funcionalidades

| Comando | Alias | Descrição |
|---|---|---|
| `.ajuda` | `.help` | Lista todos os comandos |
| `.fullbright` | `.fb` | Toggle visão noturna (só no seu client) |
| `.freecam` | `.fc` | Toggle câmera livre (você fica parado no server) |
| `.nochat` | `.nc` | Toggle silenciar chat, títulos e sons de nota |
| `.coords` | `.co` | Mostra X, Y, Z atual |
| `.ping` | — | Ping estimado com o servidor (RTT via NetworkStackLatency) |
| `.time` | `.hora` | Hora e data do sistema |
| `.uptime` | `.up` | Tempo conectado na sessão |
| `.clip [n]` | `.cl` | Teleporta N blocos acima (padrão 3, máx 256) |
| `.server` | `.srv` | Mostra servidor atual e endereço |
| `.stop` | — | Para o proxy (requer confirmação em 10s) |

## Como funciona

O proxy cria um servidor local (`0.0.0.0:19132`) que espelha o servidor real. Quando o jogador conecta, o proxy:

1. **Autentica** via Microsoft OAuth2 com o servidor real.
2. **Intercepta** todo o tráfego de pacotes em duas goroutines concorrentes:
   - **Cliente → Servidor:** pacotes de movimento, chat e ações do jogador.
   - **Servidor → Cliente:** pacotes de game state, chat, efeitos, títulos.
3. **Modifica ou bloqueia** pacotes seletivamente conforme os comandos ativos.
4. **Comandos** (precedidos por `.`) são consumidos pelo proxy e nunca chegam ao servidor.

```
Minecraft (127.0.0.1:19132)
       ↓
┌──────────────────┐
│  PL Proxy        │  ← intercepta, modifica, roteia
│  listener:19132  │
└──────┬───────────┘
       ↓
┌──────────────────┐
│  Servidor real   │  ← não sabe que existe um proxy
│  (ex: donutsmp)  │
└──────────────────┘
```

### FreeCam

- **Ativar:** o proxy envia `SetPlayerGameType(6)` (Spectator) **apenas para o client**. No servidor, você continua parado porque pacotes de movimento (`MovePlayer`, `PlayerAuthInput`, `Animate`, `InventoryTransaction`, `Interact`, etc.) são bloqueados.
- **Desativar:** o gamemode original é restaurado localmente e o servidor recebe um `MovePlayer` (teleport) de volta para a posição salva.

### Fullbright

Aplica `MobEffect` de Night Vision (EffectType 16) com duração `MaxInt32` **somente no client**. O servidor não enxerga nenhum efeito — sem risco de detecção.

### NoChat

Bloqueia pacotes `Text` (chat), `SetTitle` e `BossEvent` do servidor, além de sons de nota (`PlaySound` contendo "note"). Mensagens do sistema e do proxy continuam visíveis.

### Ping

Mede RTT (Round-Trip Time) enviando `NetworkStackLatency` ao servidor e aguardando a resposta com timeout de 5s. A medição é feita via canal sincronizado entre as goroutines de leitura/escrita.

### Clip

Lê a posição salva do jogador, envia `MovePlayer` (teleport) com deslocamento no eixo Y tanto para o servidor quanto para o client, e atualiza a posição em cache.

---

## Requisitos

- Go 1.24+
- Conta Microsoft/Xbox vinculada ao Minecraft Bedrock

## Instalação

```bash
git clone https://github.com/Olavo635/plproxy
cd plproxy
go mod tidy
go build -o plproxy .
```

## Uso

1. **Execute o proxy:**
   ```bash
   cd ~/plproxy
   ./plproxy
   ```
2. Na **primeira execução**, ele abrirá um link no terminal para autenticação Microsoft. Faça login e copie o código.
3. O token é salvo em `token.json` — nas próximas execuções o login é automático.
4. No Minecraft Bedrock, adicione um servidor:
   - **Endereço:** `127.0.0.1`
   - **Porta:** `19132`
5. Entre no servidor e use `.ajuda` no chat.

## Estrutura do Projeto

```
plproxy/
├── main.go          # Entrypoint, autenticação OAuth2, seleção de servidor
├── proxy/
│   └── proxy.go     # Listener, sessão, interceptação de pacotes e comandos
├── go.mod
└── README.md
```

## Notas

- O proxy usa **autenticação online** (Microsoft Live) — sua conta real, sem modo offline.
- O token OAuth2 é salvo em `token.json` (permissão 0600) e renovado automaticamente em background.
- Nenhum pacote é injetado no servidor sem sua ação — apenas o FreeCam e o Clip enviam pacotes modificados ao servidor.
- Dependências: `gophertunnel` (protocolo Minecraft), `oauth2` (autenticação Microsoft), `mathgl` (vetores 3D).
