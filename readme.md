# Jungle Gaming — serviço financeiro em Go

API HTTP e consumer SQS para BET, WIN, LOSS, REFUND e ROLLBACK, compostos com
Uber Fx. PostgreSQL coordena saldo, ledger imutável, idempotência, inbox, outbox
e recuperação de referências. Keycloak autentica providers e serviços internos.

**Situação da auditoria: NOT READY.** O controle IAM do broker não foi comprovado
no LocalStack Community. A autenticação OIDC da API/consumer foi testada; ela não
substitui a autorização de quem pode publicar no broker. Veja [auditoria](docs/AUDIT.md).

## Início rápido

Requer Go 1.26+, Docker e Compose v2. Para `-race`, CGO e compilador C; há alternativa
Linux Docker em [DEVELOPMENT.md](docs/DEVELOPMENT.md). Credenciais locais são somente DEV.

```sh
docker compose up -d --build --wait postgres localstack keycloak
export DATABASE_URL='postgres://jungle:jungle-local@localhost:5432/jungle?sslmode=disable'
go run ./cmd/migrate up
docker compose up -d --build --wait api
curl -fsS http://localhost:8080/health/live
curl -fsS http://localhost:8080/health/ready
```

No PowerShell, substitua `export` por `$env:DATABASE_URL = 'postgres://jungle:jungle-local@localhost:5432/jungle?sslmode=disable'`.
O CLI de migrations exige somente DATABASE_URL; a API exige também as variáveis
OIDC/mensageria. Compose já configura a API. Execução no host e `.env.example`
estão em [DEVELOPMENT.md](docs/DEVELOPMENT.md); Go não carrega `.env` automaticamente.

Filas FIFO e realms/identidades são provisionados automaticamente. Obtenção de
token, criação de wallet com `wallet-internal` e BET com `provider-alpha`:
[exemplo autenticado completo](docs/HTTP.md#exemplo-local-completo).

## Garantias e arquitetura

Money usa centavos `int64`, BRL e entrada canônica com duas casas, com overflow
verificado. O lock é por carteira no PostgreSQL; processos não dependem de memória
compartilhada. O commit inclui saldo, resultado original, ledger e eventos; na
entrada SQS inclui inbox. Publishers enviam snapshots após commit, com eventId
estável e garantia at-least-once. Referências pendentes têm retry persistente.

Providers acessam apenas suas transações. Carteira, ledger e reconciliação exigem
identidade interna. Reconciliation lê snapshot consistente e não modifica saldo.
[ARCHITECTURE.md](ARCHITECTURE.md) registra decisões, limites e shutdown.

## Verificação

```sh
go test ./...
go test -race ./...
go vet ./...
# Após preparar as dependências e variáveis descritas em DEVELOPMENT.md:
go test -race -tags=integration -count=1 ./...
git diff --check
```

Integrações usam PostgreSQL, LocalStack e Keycloak reais, schemas/filas isolados,
três processos independentes e simulações das janelas de falha. A auditoria do
zero usa [compose.audit.yaml](compose.audit.yaml), com projeto/volume/portas próprios;
comandos em [DEVELOPMENT.md](docs/DEVELOPMENT.md#infraestrutura-isolada-para-auditoria).
`go run ./cmd/migrate down` remove o schema financeiro **e seus dados**; testes
DOWN/UP usam schemas temporários. Não execute DOWN no ambiente com dados a preservar.

## Documentação

- [Enunciado original preservado integralmente](docs/CHALLENGE.md)
- [Auditoria: matriz, evidências, correções e pendências](docs/AUDIT.md)
- [Execução, configuração e testes](docs/DEVELOPMENT.md)
- [Domínio](docs/DOMAIN.md), [PostgreSQL/migrations](docs/POSTGRES.md)
- [HTTP](docs/HTTP.md), [auth e IAM](docs/AUTH.md)
- [Inbox/SQS/DLQ](docs/INBOX.md), [outbox](docs/OUTBOX.md)
- [Referências](docs/REFERENCES.md), [retry/reversões](docs/REFERENCE_RETRY.md)
- [Logs, métricas e health](docs/OBSERVABILITY.md)
