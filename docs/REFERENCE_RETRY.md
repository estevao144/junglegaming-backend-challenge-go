# Referências e reversões — Parte 5B

## Política financeira escolhida antes da implementação

Uma BET pode ter apenas uma reversão processada, REFUND **ou** ROLLBACK.
O histórico permanece definitivo: desfazer um REFUND não libera a BET para
outro REFUND ou ROLLBACK direto. Isso impede devolver duas vezes o mesmo débito.

Exemplos sobre saldo inicial 100.00:

- BET 25.00 → 75.00; ROLLBACK da BET → 100.00; REFUND da BET é rejeitado.
- BET 25.00 → 75.00; REFUND → 100.00; ROLLBACK direto da BET é rejeitado.
- BET 25.00 → 75.00; REFUND → 100.00; ROLLBACK do REFUND → 75.00.
  Uma segunda devolução da BET continua rejeitada.

Cada WIN ou REFUND também admite somente um ROLLBACK processado.
O PostgreSQL protege essa política com índice único parcial por referência
interna entre REFUND/ROLLBACK processados. Rejeições não consomem essa unicidade.

ROLLBACK exige referência PROCESSED de tipo BET, WIN ou REFUND. Provider,
player, wallet, currency, round e valor integral precisam concordar. BET gera
CREDIT; WIN e REFUND geram DEBIT. LOSS, OPENING e ROLLBACK são inválidos.
Saldo insuficiente no débito gera REVERSAL_INSUFFICIENT_BALANCE, distinto de
INSUFFICIENT_BALANCE usado para BET. Referência existente não processada gera
REFERENCE_NOT_PROCESSED; contexto/tipo/valor inválidos geram INVALID_REFERENCE.

## Persistência e coordenação

`reference_retries` guarda transaction_id, attempts, next_attempt_at,
claimed_by e claim_until, separados do input financeiro imutável.
Após resolução, attempts e completed_at ficam disponíveis para auditoria;
o claim é limpo e a operação terminal deixa de ser elegível.
A migration 0005 também agenda pendings legados da Parte 5A.
O claim usa FOR UPDATE SKIP LOCKED e termina antes da resolução.

Ordem da resolução: wallet FOR UPDATE → WagerTransaction FOR UPDATE → retry
FOR UPDATE com revalidação de token/lease → leitura da referência imutável.
Requests também começam pela wallet antes de inserir a operação. Claims não
travam wallets nem transações financeiras. Não há lock global.

O worker aplica a mesma função financeira usada por requests, na mesma
WagerTransaction. Wallet, ledger, resultado original, estado, retry e eventos
outbox são confirmados juntos. Inbox original permanece imutável.

## Retry e recuperação

Defaults: lote 5, polling 1s, lease 30s, timeout de resolução 5s, base 5s,
teto 5m e máximo 10 tentativas sem referência. Não há TTL adicional.
`delay(n) = min(base × 2^(n−1), teto)`, calculado sem ponto flutuante e sem
overflow. Entrada no pending inicia attempts=0; a primeira tentativa é imediata.
Cada tentativa sem referência incrementa attempts persistentemente. A décima
finaliza como REJECTED/REFERENCE_NOT_FOUND, sem movimento. Uma referência
encontrada na última tentativa ainda pode ser processada.
Tentativas que encontram a referência também ficam contadas na auditoria.

Variáveis: REFERENCE_BATCH_SIZE, REFERENCE_POLL_INTERVAL, REFERENCE_LEASE,
REFERENCE_PROCESS_TIMEOUT, REFERENCE_RETRY_BASE, REFERENCE_RETRY_MAX e
REFERENCE_MAX_ATTEMPTS. Os limites validados estão em
[DEVELOPMENT.md](DEVELOPMENT.md) e os exemplos em `.env.example`.

Erros transitórios de infraestrutura desfazem a tentativa inteira; o lease
expira e permite recuperação, sem consumir o orçamento de referência ausente.
Um token vencido não autoriza resolução. Depois de validado sob lock do retry,
esse lock protege o trabalho até commit, mesmo se o relógio ultrapassar o lease.

Crash antes do commit não deixa efeitos parciais. Crash após commit não reaplica
dinheiro: uma nova tentativa observa estado terminal. Restart precisa apenas do
PostgreSQL; tokens são descartáveis e leases expirados são recuperáveis.
Fx inicia polling no startup e cancela claims/trabalho e aguarda a goroutine no
shutdown, antes de fechar PostgreSQL.

Retries não recriam WagerTransactionPendingReference. Resolução produz
WagerTransactionProcessed + WalletBalanceChanged, ou WagerTransactionRejected,
via outbox. Correlation/causation originais são preservados. SQS usa o mesmo
serviço e pode remover a entrega pending após commit, sem esperar o worker.

## Testes e execução

Aplique `go run ./cmd/migrate up` com DATABASE_URL configurado antes do startup.
O runner DOWN reverte todas as versões e remove dados; valide somente em schema
temporário. 0001–0004 continuam intactas. UP/DOWN/UP e backfill de pendings legados
estão cobertos por testes reais.

`go test ./...`, `go test -race ./...` e `go vet ./...` verificam o código.
Com TEST_DATABASE_URL e TEST_SQS_ENDPOINT conforme INBOX.md, execute
`go test -race -tags=integration -count=1 ./...`. O container Go Linux documentado
em OUTBOX.md fornece GCC/CGO para race quando o host não tiver compilador C.

As integrações cobrem três tipos de ROLLBACK, replays, rejeições financeiras,
reversões concorrentes, política conjunta, constraints SQL, backoff/limite,
workers independentes, lease vencido, restart Fx, falha antes do commit e
reexecução após commit, inbox histórica, SQS e publicação de eventos.
