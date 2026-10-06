# PostgreSQL — Parte 3

## Preparação e migrations

```sh
docker compose up -d --wait postgres
```

No PowerShell, para executar Go no host:

```powershell
$env:DATABASE_URL = 'postgres://jungle:jungle-local@localhost:5432/jungle?sslmode=disable'
go run ./cmd/migrate up
```

No Bash:

```sh
export DATABASE_URL='postgres://jungle:jungle-local@localhost:5432/jungle?sslmode=disable'
go run ./cmd/migrate up
```

Versão inicial: `0001_financial.up.sql` / `0001_financial.down.sql`.
Na Parte 4A, UP aplica também `0002_outbox_delivery`, sem alterar a 0001.
DOWN reverte todas as versões em ordem inversa. O publisher exige migrations
aplicadas antes de iniciar; veja [OUTBOX.md](OUTBOX.md).
O comando registra versão e SHA-256 do arquivo UP em `schema_migrations`.
Repetir UP é seguro; alterar uma migration já aplicada provoca erro de checksum.
As mudanças são atômicas e um advisory lock coordena somente execução de migrations.
Não existe lock global no processamento de carteiras.

Reversão explícita:

```sh
go run ./cmd/migrate down
```

DOWN remove as tabelas financeiras, outbox, inbox e seus dados. Use apenas quando essa
remoção for desejada e com a aplicação parada. Os testes verificam DOWN em schemas
temporários exclusivos, sem remover o schema de uso normal.
A API não aplica migrations automaticamente. O banco deve ser preparado antes
de chamar os casos de uso financeiros.

A Parte 4B acrescenta `0003_inbox`, com resolução no mesmo commit financeiro.
Migrations 0001/0002 não mudam. Veja [INBOX.md](INBOX.md).

## Casos de uso disponíveis

`application.FinancialService` é composto por Fx e pode ser injetado nos futuros
adaptadores. Ainda não há endpoint HTTP financeiro. A API existente expõe health checks.

- `OpenWallet(ctx, playerID, balance, correlationID)`: criação interna, versão 1.
  Zero cria somente a carteira; positivo cria também OPENING, crédito no ledger
  e dois eventos, sem elevar a versão a 2.
- `Process(ctx, ProcessCommand)`: BET, WIN sem referência, LOSS, REFUND e ROLLBACK integrais.
  O command preserva a chave recebida, recebe Money validado e exige correlationId.
  WIN com referência retorna `ErrUnsupportedOperation`.
  REFUND resolve BET por provedor/ID externo ou persiste PENDING_REFERENCE,
  resolvido depois por ReferenceWorker. ROLLBACK usa o mesmo fluxo e persiste
  referência interna, direção inversa e resultado original; veja
  [REFERENCE_RETRY.md](REFERENCE_RETRY.md).
- `Store.GetWallet`, `GetLedger`, `GetTransaction` e `GetExternalTransaction`
  consultam e reidratam os modelos. Consultas externas de transação filtram providerId.
  A consulta de ledger é interna e ainda não implementa o cursor opaco da API futura.

Os adaptadores deverão autenticar e autorizar identidades antes de chamar os casos
de uso. OpenWallet é interno; o providerId de Process deve vir da identidade validada.
Sem autenticação, esses casos de uso não devem ser expostos como rotas públicas.

## BET e transação SQL

1. Validar command, Money e tipo, calcular hash e criar entidade PENDING em memória.
2. BEGIN com READ COMMITTED, consultar as identidades persistentes para replay rápido.
3. SELECT da carteira FOR UPDATE; validar jogador e moeda.
4. Inserir PENDING com ON CONFLICT DO NOTHING. Se outra execução venceu, consultar
   novamente e validar chave, ID externo e hash, retornando o resultado original.
5. Aplicar Debit no domínio. Saldo insuficiente resulta em REJECTED com
   `INSUFFICIENT_BALANCE`, saldo/version originais e WagerTransactionRejected.
6. No sucesso, gravar PROCESSED e resultado; atualizar carteira com verificação
   da versão anterior; inserir ledger DEBIT e os dois eventos na outbox.
7. COMMIT. Qualquer erro de infraestrutura faz ROLLBACK de todas as escritas.

O lock é adquirido antes do INSERT para evitar upgrade de locks KEY SHARE
adquiridos pelas FKs para FOR UPDATE. Ele existe apenas durante essa transação.
Carteiras distintas não compartilham locks. Nenhum mutex local protege a integridade.

Não há commit intermediário de PENDING neste processamento síncrono. Uma interrupção
antes do commit permite reenvio; depois do commit, reenvio lê a operação terminal.
Erro de commit pode ser ambíguo para o cliente: reenvie a mesma identidade para
consultar o resultado, sem gerar uma nova operação. Process não faz retry automático;
ReferenceWorker agenda e resolve apenas PENDING_REFERENCE, em transações separadas.

## Idempotência e hash

Constraints UNIQUE por `(provider_id, idempotency_key)` e
`(provider_id, external_transaction_id)` participam da corrida concorrente.
Não se depende de um SELECT prévio. Depois de ON CONFLICT DO NOTHING, um SELECT
separado em READ COMMITTED observa o vencedor confirmado.

Mesma chave/ID/hash retorna `IdempotentReplay=true`, com transactionId, saldo e
versão originais. Chave com payload diferente ou ID externo com outra chave
retorna `postgres.ErrConflict`, sem novas movimentações ou eventos.

SHA-256 cobre JSON UTF-8 de um struct com chaves declaradas em ordem lexical:
`externalTransactionId`, `gameId`, `kind`, `money`, `playerId`, `providerId`,
`referenceExternalTransactionId`, `roundId`, `walletId`.
Money serializa `amount` e `currency` nessa ordem, com decimal de duas casas.
Referência ausente é representada por string vazia. Usa-se o escaping padrão de
`encoding/json`, sem espaços nem newline. Não se afirma conformidade com RFC 8785.
O teste fixa os bytes canônicos esperados. Chave de idempotência, IDs internos,
correlationId, causationId e timestamps de transporte não fazem parte do hash.
Os futuros adaptadores HTTP/SQS devem chamar essa mesma função.

## Schema e outbox

Money é BIGINT em centavos + currency BRL; versão e resultado também são BIGINT.
TIMESTAMPTZ guarda instantes UTC. Equações de ledger usam NUMERIC apenas no CHECK
para evitar overflow intermediário, mantendo representação integral em BIGINT.

Constraints impõem saldo não negativo, versão positiva, carteira única por jogador
e moeda, uma OPENING por carteira, unicidade do ledger, metadados internos/externos
distintos, FKs coerentes e equação do lançamento.
Constraint triggers diferidas exigem no commit o ledger correspondente à abertura
positiva e a cada alteração de saldo/version, além de ledger para transações
processadas com movimento. Um índice único protege a versão financeira por carteira.
Assim, um UPDATE direto de saldo positivo sem o lançamento também é rejeitado.
Triggers impedem UPDATE/DELETE/TRUNCATE do ledger, alterações de entrada ou resultado
terminal de transações e edição dos snapshots da outbox.
O proprietário/superusuário do banco pode remover proteções: em produção, o usuário
da aplicação deve ter privilégios de DML, sem poderes de DDL ou de desabilitar triggers.

Tipos concretos: WagerTransactionProcessed, WagerTransactionRejected e
WalletBalanceChanged. O envelope contém identidade, tipo, agregado, correlação,
causação opcional, timestamp UTC, versão 1 e data tipado. Money é string decimal.
OPENING não inclui providerId ou ID externo inaplicáveis. LOSS gera somente
WagerTransactionProcessed. Rejeição não cria ledger ou WalletBalanceChanged.

Eventos são serializados e inseridos na mesma pgx.Tx do saldo e ledger. A Parte 3
não envia mensagens. A Parte 4A usa attempts, next_attempt_at, published_at e
locked_by/locked_until no publisher separado, com claims concorrentes e retry.
A migration 0002 adiciona ordem de envio e índice por carteira; detalhes e testes
estão em [OUTBOX.md](OUTBOX.md).

## Testes reais

Com o PostgreSQL do Compose disponível, no PowerShell:

```powershell
$env:TEST_DATABASE_URL = 'postgres://jungle:jungle-local@localhost:5432/jungle?sslmode=disable'
go test ./...
go test -tags=integration -count=1 -v ./internal/application
go vet ./...
```

A suite cria um schema aleatório por teste, aplica as migrations e remove apenas
esse schema no cleanup. Exige permissão CREATE SCHEMA na base de testes. Não usa
mocks, não pula testes por falta de conexão e não precisa de SQS/Keycloak.
O teste de lifecycle de infraestrutura da Parte 1 permanece separado:
`go test -tags=integration ./internal/app` exige PostgreSQL e SQS preparados.

Cobertura de cenários: abertura zero/positiva e conflito, BET/WIN/LOSS, replay em
nova instância, conflitos de chave/payload e ID externo, consulta isolada por
provider, ledger/outbox, rollback provocado no segundo evento, constraints,
imutabilidade, cancelamento e carteiras independentes.
Os testes críticos verificam duas apostas 80.00 sobre 100.00 e 50 chamadas da mesma
aposta com três pools. Outro teste inicia três processos independentes, cada um com
seu pool e memória, e dispara 51 chamadas simultâneas da mesma aposta.

No Windows sem GCC, execute -race no container Go com GCC:

```powershell
docker run --rm --network junglegaming-backend-challenge-go_default `
  --mount "type=bind,source=$($PWD.Path),target=/src,readonly" `
  --mount type=volume,source=jungle-go-modcache,target=/go/pkg/mod `
  --mount type=volume,source=jungle-go-buildcache,target=/root/.cache/go-build `
  -w /src -e 'TEST_DATABASE_URL=postgres://jungle:jungle-local@postgres:5432/jungle?sslmode=disable' `
  golang:1.26.0-bookworm sh -c 'go test -race ./... && go test -race -tags=integration -count=1 ./internal/application && go vet ./...'
```

Execute na raiz do projeto. Se o nome do projeto Compose mudar, ajuste a rede
usando `docker network ls`. Em um host com CGO/GCC disponíveis, também é possível
executar `go test -race ./...` e `go test -race -tags=integration ./internal/application`.
