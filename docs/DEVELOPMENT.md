# Execução das Partes 1, 2, 3, 4A, 4B, 5A, 5B, 6A e 6B

## Pré-requisitos

Go 1.26.0 ou superior, Docker com Docker Compose v2. Para `go test -race`,
habilite CGO e tenha um compilador C disponível (no Windows, GCC compatível).
O Dockerfile compila com Go 1.26.0; `go.mod` declara a mesma versão mínima.
Os checks completos da Parte 6B foram executados com Go 1.26.0/GCC em Linux Docker;
checks unitários também foram executados no Windows com Go 1.27.1.

## Ambiente completo

A Parte 5B acrescenta a migration 0005, ROLLBACK e ReferenceWorker;
veja [REFERENCE_RETRY.md](REFERENCE_RETRY.md). Aplique UP antes de iniciar a API.

Em checkout limpo, primeiro execute `docker compose up -d --wait postgres localstack keycloak`,
configure DATABASE_URL e aplique `go run ./cmd/migrate up`. Em seguida:

```sh
docker compose up --build
```

São iniciados API (8080), PostgreSQL (5432), LocalStack (4566) e Keycloak (8081).
PostgreSQL e as filas precisam estar disponíveis antes de a API iniciar.
Compose espera Keycloak saudável; a API valida discovery e a credencial de mensageria
no startup. Configuração e exemplos autenticados estão em [AUTH.md](AUTH.md).

O script da imagem LocalStack cria automaticamente:

- `wager-transactions.fifo`: visibility timeout de 30s, long polling de 20s,
  deduplicação explícita, redrive para DLQ após cinco recebimentos.
- `wager-transactions-dlq.fifo`: destino das falhas de entrada.
- `wager-events.fifo`: destino dos eventos de integração da outbox.

O consumer da Parte 4B trata operações na fila de entrada, com inbox, redelivery e
DLQ; veja [INBOX.md](INBOX.md). O publisher da Parte 4A envia snapshots para a
fila de eventos com identidade estável e retries; veja [OUTBOX.md](OUTBOX.md) para
variáveis, execução e testes PostgreSQL + LocalStack. LocalStack usa credenciais
fictícias, sem isolamento IAM demonstrado.
As filas são reprovisionadas ao iniciar o container; só PostgreSQL tem volume persistente.

```sh
curl http://localhost:8080/health/live
curl http://localhost:8080/health/ready
docker compose down
```

Liveness retorna `200 {"status":"ok"}` sem consultar dependências.
Readiness retorna `200 {"postgres":"ok","sqs":"ok"}` quando ambas respondem,
ou `503` com `unavailable` na dependência que falhou. As respostas não expõem
erros de conexão ou credenciais. POST /wagering/transactions exige Bearer e provider
autorizado; demais endpoints financeiros ficam para a Parte 6B.

## API fora do Docker

```sh
docker compose up -d postgres localstack keycloak
```

No PowerShell, importe os valores locais de `.env.example` na sessão e execute:

```powershell
Copy-Item .env.example .env
Get-Content .env | ForEach-Object {
    if ($_ -match '^([A-Z_][A-Z0-9_]*)=(.*)$') {
        [Environment]::SetEnvironmentVariable($matches[1], $matches[2], 'Process')
    }
}
go run ./cmd/api
```

No Bash:

```sh
cp .env.example .env
set -a
. ./.env
set +a
go run ./cmd/api
```

A aplicação lê apenas variáveis do processo, sem carregar `.env` automaticamente.
O Compose configura os endereços da rede dos containers diretamente; os endereços
de `.env.example` são para execução no host.

| Variável | Padrão / requisito |
| --- | --- |
| `HTTP_ADDR` | `:8080`; host:porta, porta 0 permitida para testes |
| `DATABASE_URL` | Obrigatória; URL PostgreSQL com banco |
| `LOG_LEVEL` | `INFO`; DEBUG, INFO, WARN ou ERROR |
| `DEPENDENCY_TIMEOUT` | `3s`; positivo, máximo `10s` por check de startup e por requisição de readiness |
| `AWS_REGION` | `us-east-1` |
| `SQS_ENDPOINT` | Vazio usa AWS; localmente `http://localhost:4566` |
| `SQS_QUEUE_NAME` | `wager-transactions.fifo` |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | Cadeia padrão do AWS SDK; `test` para LocalStack |
| `OIDC_ISSUER_URL` | Obrigatória; issuer exato do realm operacional |
| `OIDC_AUDIENCE` | Obrigatória; exemplo local jungle-api |
| `OIDC_INTERNAL_URL` | Opcional; origem interna do IdP na rede Docker |
| `MESSAGING_CLIENT_ID` | Obrigatória; wagering-messaging local |
| `MESSAGING_CLIENT_SECRET` | Obrigatória; secret somente DEV em .env.example |
| `REFERENCE_BATCH_SIZE` | `5`; de 1 a 20 |
| `REFERENCE_POLL_INTERVAL` | `1s` |
| `REFERENCE_LEASE` | `30s`; maior que lote × timeout de resolução + DEPENDENCY_TIMEOUT |
| `REFERENCE_PROCESS_TIMEOUT` | `5s`; máximo 10s |
| `REFERENCE_RETRY_BASE` | `5s` |
| `REFERENCE_RETRY_MAX` | `5m`; pelo menos a base |
| `REFERENCE_MAX_ATTEMPTS` | `10`; de 1 a 1000; sem TTL adicional |

Durações de referências aceitam de 1us a 24h, respeitando os limites acima.

Configuração inválida ou falha na conexão inicial impede a abertura do servidor.
A API não provisiona infraestrutura nem aplica migrations.

## Keycloak local

O realm `jungle` é importado automaticamente em um container novo.
Console: `http://localhost:8081`, administrador `admin` / `admin-local`.
Clients confidenciais com service accounts incluem:

| Client | Secret local | Role / claim |
| --- | --- | --- |
| `provider-a` | `provider-a-local` | `wagering-provider`, `provider_id=provider-a` |
| `wallet-internal` | `wallet-internal-local` | `wallet-internal` |

Os tokens têm audience `jungle-api`. Exemplo de obtenção de token:

```sh
curl -X POST http://localhost:8081/realms/jungle/protocol/openid-connect/token \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -d 'grant_type=client_credentials&client_id=provider-a&client_secret=provider-a-local'
```

Esse comando obtém token real usado pela API. Consulte [HTTP.md](HTTP.md) para
o fluxo completo com provider-alpha e wallet-internal. As credenciais são exemplos
exclusivos de desenvolvimento. Keycloak usa `start-dev` com seu banco de desenvolvimento.

## Verificação

```sh
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/api
```

Os testes padrão verificam validação de configuração, grafo Fx, HTTP,
readiness e fechamento do listener e da goroutine do servidor.
Também cobrem Money, Wallet, WagerTransaction e WalletLedgerEntry. Esses testes
de domínio não precisam de containers nem de variáveis de ambiente.

```sh
go test ./internal/domain
go test -cover ./internal/domain
```

Veja [DOMAIN.md](DOMAIN.md) para exemplos e decisões de modelagem.
Checks substituídos nos testes unitários não substituem o teste com infraestrutura real.

Para verificar startup, readiness e shutdown com PostgreSQL e SQS reais:

1. Execute `docker compose up -d postgres localstack keycloak` e aguarde as dependências.
2. Importe `.env` conforme descrito acima.
3. Execute `go test -tags=integration ./...` ou `go test -race -tags=integration ./...`.

As integrações cobrem OIDC real, HTTP, persistência financeira, três processos,
idempotência, referências, inbox/outbox, SQS/DLQ, reconciliação e métricas.

## Migrations e testes financeiros

Com DATABASE_URL definido, execute `go run ./cmd/migrate up`. A reversão
`go run ./cmd/migrate down` remove as tabelas financeiras e seus dados.

Para a suíte completa, prepare PostgreSQL, SQS e Keycloak, defina TEST_DATABASE_URL,
TEST_SQS_ENDPOINT e as variáveis OIDC/mensageria indicadas em [AUTH.md](AUTH.md).
Execute `go test -race -tags=integration -count=1 ./...`. Para testes com -race no container Go e detalhes do schema,
consulte [POSTGRES.md](POSTGRES.md) e [migrations/README.md](../migrations/README.md).
