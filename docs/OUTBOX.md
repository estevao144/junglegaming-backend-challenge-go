# Transactional Outbox — Parte 4A

O FinancialService continua gravando os eventos junto de saldo, transação e ledger
no mesmo commit. O publisher separado lê apenas registros confirmados. Assim, uma
falha no SQS não desfaz a operação financeira nem perde seu evento.

## Claim e publicação

`OutboxDelivery.Claim` usa uma transação curta com `FOR UPDATE SKIP LOCKED`, grava
um token aleatório em `locked_by` e o lease em `locked_until`, e confirma antes de
devolver o lote. Nenhuma transação ou lock PostgreSQL acompanha `SendMessage`.
O índice existente de disponibilidade e o novo índice por carteira/ordem atendem
a seleção. Claims expirados voltam a ser elegíveis pelo relógio do PostgreSQL.

A migration **0002_outbox_delivery** mantém a 0001 intacta, acrescenta
`delivery_order` e `wallet_id` derivado do snapshot. Só o primeiro evento não
publicado de cada carteira avança; um retry bloqueia essa carteira, enquanto
outras continuam. A ordem de novos eventos é a ordem de inserção, mesmo com
timestamps iguais. Eventos legados são ordenados por `occurred_at, event_id`:
a Parte 3 não persistia a ordem entre eventos com timestamp idêntico.

Cada envio renova seu lease antes de chamar o SQS, evitando que o tempo de espera
no lote consuma todo o prazo. Confirmação e retry exigem o token atual e lease
válido; um dono antigo não consegue alterar o claim de outro processo.

O corpo da mensagem é o JSONB persistido, sem reconstruir a carteira ou modificar
o envelope. Valores monetários continuam strings; nenhum ponto flutuante é usado.
`MessageGroupId` é SHA-256 hexadecimal de `data.walletId`, comum aos tipos de
evento. `aggregateId` de eventos de transação identifica a transação, por isso não
serve para agrupar todos os eventos da carteira. `MessageDeduplicationId` é SHA-256
hexadecimal de `eventId`. Os hashes acomodam identificadores arbitrários no limite
de 128 caracteres do SQS e permanecem estáveis em todas as tentativas.

## Falhas e shutdown

A garantia é **publicação at-least-once**, não exactly-once. Se SendMessage funcionar
e o processo morrer antes de `published_at`, outro publisher recupera o lease e
reenvia o mesmo eventId, payload e deduplication ID. A janela de deduplicação FIFO
não substitui idempotência no consumidor futuro. Uma chamada que exceda o lease
também pode gerar duplicatas; ownership impede confirmação pelo dono antigo.

Falhas de envio incrementam `attempts`, liberam o claim e persistem
`next_attempt_at` com backoff `base × 2^(attempts−1)`, limitado pelo máximo, sem
overflow. Não há descarte por número de tentativas. Falhas ao confirmar ou gravar
retry deixam o lease para recuperação. Registros publicados permanecem no banco.
Logs incluem eventId, eventType, tentativa e resultado, sem payload ou credenciais.

Fx valida banco/schema e fila antes de iniciar a goroutine. No shutdown, cancela
novos claims e permite concluir o envio em andamento dentro do prazo de Stop.
Ao esgotar o prazo, cancela o trabalho e aguarda a goroutine terminar antes de
fechar SQS e PostgreSQL. Claims restantes expiram; eventos não são apagados.

## Execução e testes

LocalStack e as filas FIFO já são provisionados por `docker/localstack/init-sqs.sh`:

```powershell
docker compose up -d --wait postgres localstack
$env:DATABASE_URL = 'postgres://jungle:jungle-local@localhost:5432/jungle?sslmode=disable'
go run ./cmd/migrate up
docker compose up -d --build api
```

Para executar a API pelo host, configure também AWS_REGION, AWS_ACCESS_KEY_ID,
AWS_SECRET_ACCESS_KEY, SQS_ENDPOINT e as duas filas conforme `.env.example`.
O SDK usa sua cadeia padrão de credenciais; as credenciais `test` são apenas locais.
O programa não carrega `.env` automaticamente. Compose lê as opções OUTBOX e o
nome da fila de eventos de `.env`; seu endpoint interno é `http://localstack:4566`.

Opções: OUTBOX_BATCH_SIZE (1–100, padrão 10), OUTBOX_POLL_INTERVAL (1s),
OUTBOX_LEASE (30s), OUTBOX_RETRY_BASE (1s), OUTBOX_RETRY_MAX (1m).
Durações devem ser positivas e até 24h; lease ≥ 3 × DEPENDENCY_TIMEOUT e máximo
de retry ≥ base. Cada operação de I/O respeita DEPENDENCY_TIMEOUT (padrão 3s).

```powershell
$env:TEST_DATABASE_URL = $env:DATABASE_URL
$env:TEST_SQS_ENDPOINT = 'http://localhost:4566'
$env:SQS_ENDPOINT = $env:TEST_SQS_ENDPOINT
$env:AWS_REGION = 'us-east-1'
$env:AWS_ACCESS_KEY_ID = 'test'
$env:AWS_SECRET_ACCESS_KEY = 'test'
go test ./...
go test -race ./...
go vet ./...
go test -race -tags=integration -count=1 -v ./...
```

Race no host exige CGO e compilador C. Em Windows sem GCC, use um container Go
Linux com GCC, por exemplo (PowerShell, a partir da raiz do repositório):

```powershell
docker run --rm --network junglegaming-backend-challenge-go_default `
  --mount "type=bind,source=$($PWD.Path),target=/src,readonly" `
  --mount type=volume,source=jungle-go-modcache,target=/go/pkg/mod `
  --mount type=volume,source=jungle-go-buildcache,target=/root/.cache/go-build `
  -w /src `
  -e DATABASE_URL='postgres://jungle:jungle-local@postgres:5432/jungle?sslmode=disable' `
  -e TEST_DATABASE_URL='postgres://jungle:jungle-local@postgres:5432/jungle?sslmode=disable' `
  -e SQS_ENDPOINT=http://localstack:4566 -e TEST_SQS_ENDPOINT=http://localstack:4566 `
  -e AWS_ACCESS_KEY_ID=test -e AWS_SECRET_ACCESS_KEY=test `
  golang:1.26.0-bookworm sh -c 'go test ./... && go test -race ./... && go vet ./... && go test -race -tags=integration -count=1 -v ./...'
```

As integrações usam schemas e filas isolados, conexões reais e dois pools
independentes. Cobrem snapshot, concorrência contando chamadas reais de envio
(não apenas mensagens após deduplicação), ausência de lock durante envio, retry,
crash após SendMessage, lease expirado, dono antigo, SKIP LOCKED, upgrade da Parte 3
e lifecycle Fx. Todos os testes financeiros anteriores continuam na mesma suíte.
