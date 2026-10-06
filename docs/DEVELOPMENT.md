# Execução das Partes 1 e 2

## Pré-requisitos

Go 1.26.0 ou superior, Docker com Docker Compose v2. Para `go test -race`,
habilite CGO e tenha um compilador C disponível (no Windows, GCC compatível).
O Dockerfile compila com Go 1.26.0; `go.mod` declara a mesma versão mínima.
Os checks locais desta entrega foram executados com Go 1.27.1 no Windows.

## Ambiente completo

```sh
docker compose up --build
```

São iniciados API (8080), PostgreSQL (5432), LocalStack (4566) e Keycloak (8081).
PostgreSQL e as filas precisam estar disponíveis antes de a API iniciar.
Keycloak pode terminar de iniciar depois da API: a Parte 1 ainda não valida tokens.

O script da imagem LocalStack cria automaticamente:

- `wager-transactions.fifo`: visibility timeout de 30s, long polling de 20s,
  deduplicação explícita, redrive para DLQ após cinco recebimentos.
- `wager-transactions-dlq.fifo`: destino das falhas de entrada.
- `wager-events.fifo`: destino previsto para eventos de integração da outbox.

As filas ainda não são consumidas. MessageGroupId, MessageDeduplicationId,
contratos de saída, retries e controle de acesso ao broker serão implementados
com os workers. LocalStack usa credenciais fictícias, sem isolamento IAM demonstrado.
As filas são reprovisionadas ao iniciar o container; só PostgreSQL tem volume persistente.

```sh
curl http://localhost:8080/health/live
curl http://localhost:8080/health/ready
docker compose down
```

Liveness retorna `200 {"status":"ok"}` sem consultar dependências.
Readiness retorna `200 {"postgres":"ok","sqs":"ok"}` quando ambas respondem,
ou `503` com `unavailable` na dependência que falhou. As respostas não expõem
erros de conexão ou credenciais. Não existem endpoints financeiros nesta etapa.

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
| `OIDC_ISSUER_URL` | Reservada para futura integração OIDC; ainda não é lida |

Configuração inválida ou falha na conexão inicial impede a abertura do servidor.
A API não provisiona infraestrutura nem aplica migrations.

## Keycloak local

O realm `jungle` é importado automaticamente em um container novo.
Console: `http://localhost:8081`, administrador `admin` / `admin-local`.
Há dois clients confidenciais de teste com service accounts:

| Client | Secret local | Role / claim |
| --- | --- | --- |
| `provider-a` | `provider-a-local` | `provider`, `providerId=provider-a` |
| `wallet-internal` | `wallet-internal-local` | `wallet-internal` |

Os tokens têm audience `jungle-api`. Exemplo de obtenção de token:

```sh
curl -X POST http://localhost:8081/realms/jungle/protocol/openid-connect/token \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  -d 'grant_type=client_credentials&client_id=provider-a&client_secret=provider-a-local'
```

Esse comando testa o IdP. A validação desses tokens e a autorização na API serão
implementadas antes de expor endpoints de negócio. As credenciais são exemplos
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

O teste de integração atual não testa OIDC, persistência financeira ou consumo
de mensagens. Testes com três processos, interrupções, idempotência, autenticação
e concorrência financeira serão adicionados nas respectivas etapas.

## Migrations

Não existem migrations aplicáveis ou reversíveis nesta etapa. O diretório
`migrations/` está reservado; os comandos de aplicação e reversão serão incluídos
com o schema financeiro. Veja [migrations/README.md](../migrations/README.md).
