# Consumer SQS e transactional inbox — Parte 4B

A Parte 6A adiciona a autenticação OIDC da service account do consumer e autorização
do provider antes de ProcessIncoming: [AUTH.md](AUTH.md). Contrato, hash, atomicidade
da inbox e acknowledgment continuam preservados.

Entrada: `wager-transactions.fifo`; saída: `wager-events.fifo`. O script LocalStack
já provisiona a entrada, `wager-transactions-dlq.fifo` e RedrivePolicy com
`maxReceiveCount=5`. O consumer não envia mensagens manualmente à DLQ.

## Contrato

```json
{
  "messageId": "delivery-envelope-123",
  "type": "WagerTransactionRequested",
  "occurredAt": "2026-10-06T12:00:00Z",
  "correlationId": "request-123",
  "data": {
    "providerId": "provider-a",
    "externalTransactionId": "bet-123",
    "idempotencyKey": "custom-key-123",
    "playerId": "player-id",
    "walletId": "wallet-id",
    "roundId": "round-1",
    "gameId": "game-1",
    "kind": "BET",
    "money": {"amount": "25.00", "currency": "BRL"}
  }
}
```

Suporta BET, WIN sem referência, LOSS (amount `"0.00"`), REFUND e ROLLBACK integrais.
Reversões exigem `data.referenceExternalTransactionId`. Veja
[REFERENCES.md](REFERENCES.md) e [REFERENCE_RETRY.md](REFERENCE_RETRY.md).
Money exige string com
duas casas e BRL, sem ponto flutuante. Wallet deve existir. JSON, envelope, campos
obrigatórios e tipos precisam ser válidos; campos desconhecidos são recusados.
`correlationId` é opcional e assume messageId quando ausente. `occurredAt` exige
RFC3339; timestamps financeiros continuam definidos pelo serviço, não pelo emissor.

O produtor deve usar MessageGroupId baseado na carteira, por exemplo SHA-256
hexadecimal de walletId, e MessageDeduplicationId baseado no messageId do envelope.
Mensagens de carteiras diferentes usam grupos diferentes. FIFO não substitui as
constraints PostgreSQL. Broker MessageId é usado nos logs; a identidade da inbox
é o **messageId do envelope**, como especifica o README.

## Atomicidade, idempotência e recuperação

`OperationConsumer -> ParseOperation -> FinancialService.ProcessIncoming`.
O mesmo `prepareOperation/processOperation` atende Process (entrada direta/futuro
HTTP) e ProcessIncoming. O consumer não implementa regras financeiras.

A migration 0003 cria `inbox_messages`, com chave primária
`(consumer_name, source, message_id)`. Source é QueueArn, estável entre host e
container; consumer_name deve permanecer igual entre réplicas/restarts. Campos:
hash SHA-256 do corpo completo, correlação, timestamps, status, transaction_id e
failure_code. FK vincula resolução financeira. Triggers proíbem alteração de
identidade/resultado e commit de inbox PENDING, e validam o vínculo financeiro.
A resolução PENDING_REFERENCE conserva o snapshot da entrega, sem impedir a
transição financeira posterior descrita em [REFERENCE_RETRY.md](REFERENCE_RETRY.md).

Inbox distingue entregas; a idempotência financeira distingue operações por
provider/chave/ID externo e hash canônico. O hash financeiro original não muda
com messageId, occurredAt, correlação ou metadados SQS. O hash separado da inbox
verifica o corpo completo em reentregas; o produtor deve reenviar o mesmo envelope.
Reutilizar messageId com corpo diferente não é replay: fica para redrive/DLQ,
preservando o registro original. Mesmo negócio com dois messageIds cria duas
resoluções de inbox e apenas uma movimentação financeira.

Uma única transação SQL insere inbox, executa financeiro, ledger e outbox e resolve
inbox antes do COMMIT. Um savepoint permite desfazer alterações financeiras
parciais e persistir rejeição terminal; RELEASE SAVEPOINT não confirma a transação
externa. Erros desconhecidos/infraestrutura e falhas no commit desfazem tudo.

**DeleteMessage ocorre apenas após COMMIT** de PROCESSED, REJECTED ou
PENDING_REFERENCE (resolução durável da entrega, aguardando ReferenceWorker).
BET sem saldo
mantém WagerTransactionRejected e seu evento, conforme Parte 3. Entrada sem modelo
financeiro válido, conflito, operação não suportada ou overflow resolve inbox como
REJECTED, sem criar uma transação financeira parcial ou inventar evento financeiro.

Timeout, PostgreSQL indisponível ou resultado de commit incerto não removem a
mensagem. JSON inválido, tipos incompatíveis ou campos obrigatórios ausentes são
poison messages: sem tentativa interna infinita e sem efeitos financeiros, seguem
a RedrivePolicy. Falha de DeleteMessage também permite reentrega segura.

Se o processo morrer depois do commit e antes do delete, visibility timeout expira.
Outro consumer encontra a inbox resolvida, verifica hash e remove a entrega sem
novo saldo, versão, ledger ou eventos. A garantia é at-least-once, com resolução
financeira idempotente e persistente.

## Polling, shutdown e configuração

Defaults em `.env.example`: 2 loops independentes, lote 1, long polling 20s,
visibility 30s e processamento até 5s; acknowledgement usa DEPENDENCY_TIMEOUT
(3s). Configuração valida visibility maior que o orçamento do lote mais margem.
O timeout HTTP do SDK cobre long polling. Não é necessário heartbeat de visibility
nesse orçamento; sobreposição excepcional permanece protegida por inbox e locks
financeiros. Após falha, reentrega usa o prazo de visibility; cinco recebimentos
sem resolução levam à DLQ. Falhas de Receive aguardam 1s antes de novo polling.

Variáveis: SQS_CONSUMER_NAME, SQS_CONSUMER_CONCURRENCY (1–10),
SQS_CONSUMER_BATCH_SIZE (1–10), SQS_CONSUMER_WAIT_SECONDS (1–20),
SQS_VISIBILITY_SECONDS (1–43200) e SQS_PROCESS_TIMEOUT (positivo, até 10s).
Endpoint, região, credenciais e nome da fila usam as configurações AWS existentes.

Fx valida infraestrutura/schema e inicia o consumer. Stop cancela ReceiveMessage
e novas entradas, permite concluir trabalho atual dentro do prazo e depois cancela
o trabalho restante. Aguarda goroutines antes de fechar SQS/PostgreSQL. Mensagens
recebidas e ainda não iniciadas voltam após visibility. Logs incluem identidades,
receive count, resultado e classificação, sem corpo financeiro ou credenciais.

## Execução e testes

```powershell
docker compose up -d --wait postgres localstack
$env:DATABASE_URL = 'postgres://jungle:jungle-local@localhost:5432/jungle?sslmode=disable'
go run ./cmd/migrate up
docker compose up -d --build api
$env:TEST_DATABASE_URL = $env:DATABASE_URL
$env:TEST_SQS_ENDPOINT = 'http://localhost:4566'
$env:SQS_ENDPOINT = $env:TEST_SQS_ENDPOINT
$env:AWS_ACCESS_KEY_ID = 'test'
$env:AWS_SECRET_ACCESS_KEY = 'test'
go test ./...
go test -race ./...
go vet ./...
go test -race -tags=integration -count=1 ./...
```

Race exige CGO e compilador C; alternativa em container Go Linux está em
[OUTBOX.md](OUTBOX.md). As integrações usam PostgreSQL e LocalStack reais,
schemas/filas isolados, visibility 1s e maxReceiveCount 2 para recuperação rápida.
Cobrem BET/WIN/LOSS, reentrega real após commit, falha diferida no commit, DLQ,
duas entregas da mesma operação, replay entre transportes, rejeições/savepoint,
constraints e lifecycle com carteiras independentes. UP/DOWN/UP é testado apenas
em schemas temporários. Migrations 0001/0002 permanecem intactas.
