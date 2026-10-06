# Parte 7 — auditoria final e hardening

Auditoria realizada em 2026-10-06 contra o [enunciado original](CHALLENGE.md).
A primeira fase foi somente leitura: código, SQL 0001–0005, configuração,
provisionamento, documentação e testes. As correções começaram após localizar
implementações/evidências e reproduzir os problemas investigados. Prompts das
etapas anteriores não foram usados para substituir requisitos do enunciado.

**Veredito: NOT READY.** Falta comprovar controle efetivo de acesso ao broker
por credenciais/policies. A suíte local passou com PostgreSQL, LocalStack e
Keycloak reais, mas esse resultado não comprova enforcement IAM.

## A) Auditoria do README

Status finais: PASS = implementação e evidência verificadas; PARTIAL = há parte
obrigatória não comprovada; MISSING = implementação ausente; QUESTIONABLE = exige
investigação. Os pontos QUESTIONABLE de WIN/banco/inbox/métricas foram reproduzidos
e corrigidos. Não há MISSING/QUESTIONABLE restante no escopo obrigatório auditado.
O único grupo PARTIAL é autorização do broker. Esta matriz inclui detalhes dos
requisitos, além dos nomes das funcionalidades.

As paths abaixo são relativas à raiz. Testes financeiros citados ficam em
`internal/application/*_integration_test.go`; unitários de entidades ficam em
`internal/domain/*_test.go`. Fixtures usam schemas/filas exclusivos, exigem
dependências reais e falham quando elas estão indisponíveis. O helper de três
processos inicia o executável de teste três vezes, com stdin como barreira,
ambiente/conexões/memória separados; não representa três goroutines de um singleton.

### Stack, composição, configuração e domínio

| Requisito | Implementação | Evidência | Status |
| --- | --- | --- | --- |
| Go/versão, Modules e dependências reproduzíveis | `go.mod`, `go.sum`, `Dockerfile`, Go 1.26.0 | Gates Go e build Docker do zero | PASS |
| Uber Fx por construtores, módulos, provide/invoke | `internal/app/app.go` e módulos config/postgres/auth/http/workers | `TestComposition`, `TestRealInfrastructureLifecycle` | PASS |
| Domínio independente de Fx/HTTP/SQS/pgx | `internal/domain`, imports somente padrão | Inspeção de imports e testes sem containers | PASS |
| Entidades encapsuladas e criação separada de reidratação | Money/Wallet/WagerTransaction/Ledger com estado privado e snapshots | `TestWalletCreationAndRehydration`, `TestTransactionOutcomesAndSnapshotsAreIndependent`, `TestLedgerCreationRehydrationAndImmutability` | PASS |
| Zero values inválidos, erros classificáveis, ausência de panic de negócio | Validações e sentinelas em `domain`; `errors.Is` na aplicação | `TestMoneyRejectsUninitializedAndMixedCurrency`, `TestWalletRejectsInvalidState`, `TestTransactionRehydrationValidation` | PASS |
| Configuração inicial e env obrigatória documentadas | `internal/config/config.go`, `.env.example`, Compose, `DEVELOPMENT.md` | `TestLoad`, `TestInvalidConfigurationFailsBeforeStartup` e startup real | PASS |
| I/O com contexto e prazo | pgx/SDK/OIDC recebem ctx; readiness, requests e workers têm limites | `TestPostgresIndependentWalletAndCancellation`, `TestReadinessPropagatesTimeout`, testes de shutdown | PASS |
| Lifecycle: início, parada observável e fechamento na ordem das dependências | Hooks Fx, canais done, cancelamento/join | `TestRealInfrastructureLifecycle`, `TestServerLifecycle`, testes dos três workers e sampler | PASS |

Dependências diretas: Fx compõe a aplicação; pgx fornece pool/SQL/transações;
AWS SDK/config/SQS assina e transporta mensagens; go-oidc valida OIDC/JWKS;
oauth2 implementa client_credentials; go-jose fabrica assinatura inválida nos
testes de segurança. Nenhuma dependência direta ficou sem uso. go.mod/go.sum
foram preservados; não foi necessário executar go mod tidy.

### Money, Wallet e abertura

| Requisito | Implementação | Evidência | Status |
| --- | --- | --- | --- |
| Valor e currency; imutabilidade | `domain/money.go`, centavos int64 privados, operações retornam valores | `TestMoneyArithmetic`, `TestMoneyComparisonNegationAndSerialization` | PASS |
| String decimal, exatamente duas casas, BRL/ISO, sem arredondamento | `NewMoney`, HTTP e parser SQS compartilham domínio | `TestMoneyParsing`, `TestMoneyRejectsInvalidInput`, testes HTTP/parsing SQS | PASS |
| Vazio, sinais, negativo externo, NaN/Infinity, científico e escala inválida rejeitados | Parsing de dígitos com ParseInt; sem ParseFloat | `TestMoneyRejectsInvalidInput`, `TestCompleteHTTPWithRealInfrastructure` | PASS |
| Zero, soma, subtração, negação, comparação e JSON canônico | Métodos de Money, amount/currency em ordem fixa | `TestMoneyArithmetic`, `TestMoneyComparisonNegationAndSerialization` | PASS |
| Overflow parsing/add/subtract/negate, inclusive MinInt64 | Verificações aritméticas e Decimal sem negar MinInt64 | `TestMoneyRejectsInvalidInput`, `TestMoneyArithmetic`, `TestMoneyComparisonNegationAndSerialization` | PASS |
| Moedas incompatíveis e valores não inicializados | `compatible`/`Validate`; operação apenas BRL | `TestMoneyRejectsUninitializedAndMixedCurrency`, `TestMoneyCannotOperateOrSerializeUnsupportedCurrency` | PASS |
| Persistência exata sem float | BIGINT + BRL; CHECK usa NUMERIC exato para equações | Busca global do código/SQL e testes PostgreSQL de saldo/ledger | PASS |
| Wallet: id/player/currency/balance/version/createdAt/updatedAt | `wallet.go`, `wallets` e reidratação do repository | `TestWalletCreationAndRehydration`, `TestPostgresWalletOpening` | PASS |
| Wallet única por jogador/moeda | UNIQUE `(player_id,currency)` na 0001 | Abertura duplicada via caso de uso/HTTP e constraints | PASS |
| Debit não deixa saldo negativo, crédito/débito positivos e moeda compatível | Métodos Wallet + CHECK/FK SQL | `TestWalletRejectedMovementsDoNotMutate`, `TestPostgresTwoIndependentBets`, constraints | PASS |
| Versão inicial 1; incremento apenas em saldo efetivamente alterado | Wallet/serviço; trigger de movimento e próximo ledger/version | `TestWalletMovements`, `TestLossProcessingDoesNotMoveWallet`, testes BET/WIN/LOSS PostgreSQL | PASS |
| Overflow de saldo/versão e timestamps válidos sem mutação parcial | Wallet valida antes de alterar campos | `TestWalletRejectedMovementsDoNotMutate`, `TestWalletRejectsInvalidState` | PASS |
| OPENING positivo interno, PROCESSED, CREDIT, dois eventos e versão 1 | `OpenWallet`, `NewOpeningTransaction`, 0001 | `TestPostgresWalletOpening`, `TestCompleteHTTPWithRealInfrastructure`, contratos dos eventos | PASS |
| Saldo inicial zero sem OPENING/ledger/eventos financeiros | Ramo explícito de abertura | `TestPostgresWalletOpening`, HTTP completo | PASS |
| OPENING externo negado e crédito inicial não duplicado | prepareOperation, domínio, metadados SQL/índice one_opening_per_wallet | `TestOpeningTransaction`, HTTP completo, `TestExternalOpeningRejectedByAuthenticatedSQSConsumer` | PASS |
| Falha na abertura desfaz wallet/transação/ledger/outbox | Mesma pgx.Tx, sem commit intermediário | `TestPositiveHTTPOpeningRollsBackOnOutboxFailure` | PASS |

Limites financeiros internos: -92233720368547758.08 a 92233720368547758.07 BRL.
Entradas externas são não negativas e canônicas; zero só é permitido na abertura
e em LOSS. BRL é a única moeda operacional; mismatch é testado sem habilitar USD.

### Transações, regras e referências

| Requisito | Implementação | Evidência | Status |
| --- | --- | --- | --- |
| Todos os identificadores externos, hash/chave recebida, wallet/player/round/game/kind/Money/timestamps | TransactionData e colunas 0001, correlação/causação 0004 | `TestTransactionRejectsInvalidInput`, processamento/leitura PostgreSQL e HTTP | PASS |
| PENDING inicial e máquina de estados explícita | `canTransitionTo`, métodos da entidade e trigger SQL | `TestTransactionTransitions` testa todos os pares/estados desconhecidos | PASS |
| PENDING → pending reference/processed/rejected/failed; pending reference → terminais; terminais imutáveis | Domínio e `protect_transaction` | Transições unitárias e `TestPostgresConstraintsAndAppendOnly` | PASS |
| Resultado original e failureCode/referência interna quando aplicáveis | Snapshot/result columns, consulta de replay | `TestPostgresBetWinLossReplayAndConflicts`, refunds/rollbacks/referências e consultas HTTP | PASS |
| Todo PENDING confirmado retomável, ou fluxo síncrono sem aceite intermediário | PENDING não é confirmado sozinho pelo caso de uso; erros fazem rollback | Atomicidade/cancelamento e reenvio em nova instância; inspeção do commit | PASS |
| BET positivo e saldo suficiente | Wallet.Debit e rejeição auditada | `TestPostgresTwoIndependentBets`, `TestTransactionKindsAndAmounts` | PASS |
| WIN positivo; referência opcional à BET da mesma rodada/contexto; payout independente | `WinReferenceFailure`, helper financeiro, trigger 0006 | `TestWinReferenceContextAndIndependentPayout`, `TestDatabaseRejectsInvalidReferencedWin`, WIN SQS pending/HTTP | PASS |
| LOSS = 0.00, moeda válida, sem saldo/versão/ledger/BalanceChanged e com Processed | `NoMovement` e evento de processamento | `TestLossProcessingDoesNotMoveWallet`, `TestPostgresBetWinLossReplayAndConflicts`, `TestInboxBetWinLossViaRealSQS` | PASS |
| REFUND integral de BET PROCESSED com contexto igual | `ReversalReferenceFailure`, serviço e trigger 0004 | `TestRefundNormalAndOriginalReplay`, `TestRefundInvalidExistingReferences`, proteção SQL | PASS |
| ROLLBACK integral de BET/WIN/REFUND e direção inversa | Domínio + triggers 0005 | `TestRollbackTypesAndOriginalReplay`, `TestRollbackInsufficientAndInvalidReferences`, proteção SQL | PASS |
| Referência externa obrigatória nas reversões; resolução por provider/ID externo | Consulta indexada provider/external e validações da entrada | `TestRefundProviderScopedLookup`, testes de parsing/refund/rollback | PASS |
| Contexto provider/player/wallet/currency/round igual; reversão integral | Validação domínio/SQL 0004–0006; FK wallet/player/currency | Testes de referências inválidas, WIN SQL e domínio | PASS |
| Não repetir reversões; combinação REFUND/ROLLBACK coerente | Índice parcial conjunto 0005; lock por wallet; histórico não libera BET já revertida | `TestRollbackAndRefundPolicyConcurrent`, `TestRefundConcurrentIndependentPools`, SQL de duplicata | PASS |
| ROLLBACK com débito sem saldo rejeitado com código distinto de BET | `REVERSAL_INSUFFICIENT_BALANCE` | `TestRollbackInsufficientAndInvalidReferences`, `TestReferenceWorkerFinancialRejections` | PASS |
| Referência ausente persistida sem movimento, inclusive via SQS | Pending + outbox + reference_retries; inbox 0006 | Refund/rollback pending e `TestReferencedWinSQSCommitsPendingAndRecovers` | PASS |
| Referência existente ainda pendente ou terminal sem sucesso tem comportamento explícito | Rejeita com REFERENCE_NOT_PROCESSED; somente referência ausente entra em espera | Regras unitárias de WIN/reversões e testes de referência rejeitada | PASS |
| Retry exponencial, attempts/nextAttempt, máximo persistente e rejeição final | reference_retries, `ReferenceBackoff`, policy, `REFERENCE_NOT_FOUND` | `TestReferenceRetryBackoffAndExhaustion`, `TestReferenceBackoffAndPolicy` | PASS |
| Claim SKIP LOCKED/lease, recuperação/restart e identidade original | ReferenceQueue/worker, ordem wallet → transaction → retry | `TestReferenceWorkersLeaseRecoveryAndConcurrentReplay`, `TestReferenceFxRestartRecovery`, `TestReferenceSameClaimExecutedConcurrently` | PASS |
| Expiração de token enquanto aguarda lock não autoriza resolução | Revalidação depois do lock pelo relógio PostgreSQL | `TestReferenceLeaseExpiresWhileWaitingForRetryLock` | PASS |
| Nenhum novo pending event por retry; conclusão atômica e inbox histórica imutável | Mesma WagerTransaction, retry/outbox no commit, inbox não é editada | `TestReferenceWorkerResolvesSameTransaction`, `TestRollbackSQSAndHistoricalInboxAfterResolution`, WIN SQS | PASS |

Falhas transitórias desfazem a transação e permitem reentrega; desconhecidas não
são classificadas como rejeição de negócio. Entrada semanticamente inválida tem
rejeição durável da inbox sem entidade parcial; poison vai à DLQ do broker.
FAILED é transição auditável de infraestrutura permanente no modelo, não destino
automático de qualquer indisponibilidade temporária. Referências usam 10 tentativas
por padrão, sem TTL adicional; a referência encontrada na última tentativa pode
ser processada. Códigos estáveis e interpretação estão em DOMAIN/REFERENCE_RETRY/HTTP.

### Concorrência, idempotência e ledger

| Requisito | Implementação | Evidência | Status |
| --- | --- | --- | --- |
| Coordenação por wallet, sem lock global financeiro/local singleton | READ COMMITTED, SELECT FOR UPDATE; update com versão anterior | `TestPostgresIndependentWalletAndCancellation`, três processos | PASS |
| Duas BETs de 80.00 sobre 100.00: um sucesso/uma rejeição/20.00/um débito | Lock e regra de saldo, proteção SQL | `TestPostgresTwoIndependentBets`, inclusive replays dos dois resultados | PASS |
| 50 entregas paralelas da mesma BET: um débito | UNIQUE provider/key e provider/external, ON CONFLICT | `TestPostgresFiftyConcurrentReplays` em três pools | PASS |
| Pelo menos três processos realmente independentes | OS exec + três Fx graphs/pools/memórias + barreira | `TestPostgresThreeIndependentProcesses`: 51 chamadas | PASS |
| Carteiras independentes progridem simultaneamente | Locks por linha; publisher também separa wallets | Testes PostgreSQL/wallets, `TestInboxFxConsumersIndependentWallets`, outbox SKIP LOCKED | PASS |
| HTTP e SQS simultâneos com equivalência financeira | Shared processOperation + wrappers autorizados | `TestConcurrentAuthenticatedHTTPAndSQSUseOneFinancialCommit`: 50 HTTP + duas entregas reais SQS, três pools | PASS |
| Idempotency-Key obrigatório/preservado, não calculado silenciosamente | DTO/command + domínio e colunas imutáveis | Hash/key unitários, HTTP sem chave, conflitos PostgreSQL | PASS |
| Hash determinístico SHA-256/JSON canônico com chaves ordenadas; campos/normalizações documentados | `PayloadHash`, Money string canônica; exclui key/transporte | `TestPayloadHashCanonicalBusinessFields`, `TestOperationMappingAndTransportHashEquivalence`, POSTGRES.md | PASS |
| Mesma chave/negócio replay; chave/payload diferente conflito; externo não reaplica com nova chave | Identidades únicas e confronto persistente | `TestPostgresBetWinLossReplayAndConflicts`, inbox conflitos, provider isolation | PASS |
| Replay mantém saldo/versão originais após outras operações/restart | Colunas result imutáveis | Replays PostgreSQL/refund/rollback, HTTP completo e API real após SIGTERM/restart | PASS |
| Saldo, versão, status, ledger, inbox e outbox atômicos | Repositories compartilham pgx.Tx; savepoint não é commit | `TestPostgresRollbackAllFinancialWrites`, `TestInboxTransientRollbackThenRedelivery`, falha de outbox em refund/abertura | PASS |
| Ledger auditável com todos os campos/equação exata/nonnegative | Entidade ledger + SQL BIGINT/NUMERIC e FKs/triggers | `TestLedgerRejectsInvalidState`, `TestLedgerCreationRehydrationAndImmutability`, testes de saldo/ledger | PASS |
| UNIQUE wallet/transaction e wallet/result version | Índices SQL 0001 | Constraints, concorrência e contagem de efeitos persistidos | PASS |
| UPDATE/DELETE/TRUNCATE de ledger bloqueados; correção só com novos lançamentos | Triggers SQL 0001 | `TestPostgresConstraintsAndAppendOnly` | PASS |
| Movimento só confirma com próximo ledger e operação coerente | Constraint triggers diferidas wallet/transaction/ledger | Constraints contra update direto de saldo/versão e testes de atomicidade | PASS |
| LOSS/rejeições sem ledger financeiro | Ramo NoMovement/rejeição + trigger ledger | BET/WIN/LOSS, rejeições de refund/rollback, inbox/savepoint | PASS |
| Saldo final confere com soma crédito menos débito | AssertBalance consulta SUM NUMERIC e compara BIGINT; reconciliação streaming | Testes financeiros/concor­rência/recovery e reconciliação real | PASS |

### Inbox, SQS, DLQ, outbox e eventos

| Requisito | Implementação | Evidência | Status |
| --- | --- | --- | --- |
| Inbox identidade `(consumerName,messageId)`, hash, recebimento/conclusão/source | 0003 + UNIQUE 0006; source validado no Get | `TestInboxMessageIdentityCannotChangeSource`, `TestInboxIdentityHashConflict`, constraints inbox | PASS |
| Inbox/financeiro compartilham commit; pending reference permite ack após persistir | ProcessIncoming + constraint de resolução, inclusive WIN | `TestInboxTransientRollbackThenRedelivery`, WIN/refund/rollback SQS | PASS |
| DeleteMessage só após commit; rejeição terminal permite ack | Consumer.Resolve não dá ack; Handle valida resultado confirmado | `TestConsumerAcknowledgementDecision`, commit failure e poison/DLQ reais | PASS |
| Crash entre commit e delete: nova instância sem movimento duplicado | Inbox durável + hash, financeiro independente do FIFO | `TestInboxCrashAfterCommitBeforeDelete`, receiveCount=2/messageId real igual | PASS |
| Duas mensagens do mesmo negócio não criam dois movimentos | Inbox por entrega e idempotência financeira separadas | `TestInboxDifferentMessagesAndCrossTransportReplay`, HTTP/SQS simultâneos | PASS |
| JSON malformado/poison/conflito envelope, entradas sem metadados | Decoder estrito; erro permanente vs rejeição sem efeitos parciais | `TestMalformedAndSemanticMessages`, `TestInboxSemanticInputAndFinancialConflicts`, DLQ real | PASS |
| Filas entrada/DLQ FIFO e RedrivePolicy automáticas | `docker/localstack/init-sqs.sh`, checks SDK | Provisionamento do zero e `TestInboxPoisonMessageRedrivesToRealDLQ` | PASS |
| Visibility, long polling, tentativas/backoff e comportamento inválido documentados | Config/SQS Receive; visibility como intervalo de retry; maxReceiveCount=5 local | `TestLoad`, DLQ/reentrega e INBOX.md | PASS |
| MessageGroupId/MessageDeduplicationId documentados; não usar FIFO como idempotência financeira | wallet/envelope na entrada; hash wallet/eventId na saída | Parsing/hash, reentrega real, contagem de chamadas publishers e OUTBOX/INBOX.md | PASS |
| SIGTERM para busca, conclui trabalho ou libera visibility; goroutines encerram | stopFetch/cancelWork/done + Release(0) com cleanup limitado | Shutdown unitário, `TestConsumerShutdownReleasesRealSQSVisibility`, exit 0 da API real | PASS |
| Outbox estável com aggregate/type/payload/occurred/attempts/next/published | 0001/0002, snapshot de construtores de domínio | Contratos eventos e `TestOutboxNormalPublication` | PASS |
| Publicação por worker separado só depois de commit | Queries enxergam somente eventos confirmados | `TestOutboxDoesNotPublishUncommittedEvent` | PASS |
| Claim/lease/SKIP LOCKED e múltiplos publishers; rede fora da SQL Tx | OutboxDelivery.Claim curto, Renew/ownership; envio separado | `TestOutboxConcurrentIndependentPublishers` conta sends e obtém NOWAIT durante Publish | PASS |
| Retry persistente exponencial sem descarte silencioso | attempts/nextAttempt/lease, Backoff | `TestOutboxTemporaryFailureDurableRetry`, `TestBackoff` | PASS |
| Crash commit antes de publicar e Send antes de MarkPublished | Lease expirado recuperado por nova instância; mesmo eventId/payload/dedup | `TestOutboxCrashAfterRealSendAndStaleOwner`, `TestOutboxLeaseExpiresAndSkipsLockedWallet` | PASS |
| Ordering por wallet sem bloquear outras | delivery_order + primeiro evento não publicado por wallet | Snapshots recebidos em ordem, SKIP LOCKED e publishers concorrentes | PASS |
| Destino/roteamento/consumo de saída documentados | wager-events.fifo provisionada, OUTBOX.md | Contrato/routing e fila real | PASS |
| Quatro eventos concretos obrigatórios | domain/events.go/reference_event.go | `TestFinancialEventContracts`, `TestPendingReferenceEventAndFutureTransition`, integração de referência | PASS |
| Envelope: eventId/type/aggregate/correlation/optional causation/UTC/version/data tipado | Construtores definem tipo/versão; JSON com Money string | Contratos domínio, snapshots e comparação exata SQS | PASS |
| WalletBalanceChanged com wallet/transaction/direction/Money/before/after/version | Struct concreto a partir do ledger/result da operação | Contrato unitário, refund/rollback event snapshot e outbox real | PASS |
| Snapshot imutável, nunca reconstruído do saldo atual, at-least-once | Trigger outbox + publisher lê payload persistido | SQL contra update, comparação de bytes e crash de publicação | PASS |

### Autenticação, autorização, HTTP e observabilidade

| Requisito | Implementação | Evidência | Status |
| --- | --- | --- | --- |
| IdP externo real, realm dedicado/client_credentials/provisionamento automático | Keycloak Compose e realms versionados | Startup do zero e testes reais de auth | PASS |
| Assinatura RS256, issuer, audience, expiração, JWKS e claims verificadas | `platform/auth/auth.go`, go-oidc; sub/azp/typ/nbf | `TestRealKeycloakProviderClaimsAndInvalidTokens`, `TestRealKeycloakTokenExpiredAtVerification`, claims unitários | PASS |
| Principal assinado determina provider; body não tem autoridade | security.Principal + middleware/wrapper financeiro | `TestAuthorizationPrecedesFinancialAccess`, `TestRealAuthenticatedFinancialHTTPAndProviderIsolation` | PASS |
| 401 ausente/inválido/expirado e 403 sem permissão; JWT fabricado/issuer/audience errados negados | RequireAuthentication/AuthorizeProvider | Auth unitário e real; ausência de efeitos conferida em SQL | PASS |
| Provider só lê/replay suas transações e namespace de identidade isolado | Queries SQL filtram provider e wrapper verifica path/body | HTTP real alpha/beta, conflitos/replays com mesmo key/external por provider | PASS |
| Carteira/ledger/reconciliation restritos ao serviço interno | wallet-internal, sem ownership artificial de provider | `TestWalletServiceRejectsProviderBeforeAnyIO`, HTTP completo 403/404 | PASS |
| Consumer tem identidade separada, role, allowlist, cache/renovação e secrets via env | MessagingIdentity/client_credentials/AuthorizeMessage | `TestRealMessagingAuthorizationBeforeInboxAndFinance`, `TestRealMessagingIdentityCacheAndRenewal`, startup com credencial errada | PASS |
| JWT/secrets fora de payload/DB/logs/domínio | Credencial só no adapter auth; logs projetam metadados | Inspeção de structs/SQL/logging, `TestCorrelationLogsExcludeCredentialsAndPropagate` | PASS |
| Acesso ao broker controlado por credenciais **e policies aplicadas** | SDK SigV4 e templates `docker/aws`, LocalStack Community sem comprovação | Não existe teste real de AccessDenied por IAM no ambiente local | **PARTIAL** |
| POST /wallets: request/response, saldo inicial e conflito | transport/http/resources.go + serviço autorizado | HTTP completo e abertura atômica com falha de outbox | PASS |
| GET /wallets/:id, ledger cursor opaco/ordem estável/limit=50 | Reads/index wallet/time/id, cursor vinculado à wallet, limite 1–200 | `TestLedgerCursorWalletAndOrdering`, paginação HTTP sem duplicata | PASS |
| GET transaction/:id e providers/:provider/external | DTO de estado/referências/códigos/resultado e isolamento | HTTP completo e auth/provider isolation | PASS |
| POST wagering, key obrigatória, replay/pending/invalid/conflict/rejected/transient distinguíveis | Handler + resposta pública sem SQL/credenciais; 200/202/400/409/422/503 | HTTP completo, `TestErrorMappingDoesNotLeakDetails`, contratos HTTP.md | PASS |
| POST reconciliation: diferença stored-calculated, cadeia/soma, snapshot consistente, sem alteração | REPEATABLE READ READ ONLY + streaming por versão | HTTP consistente/divergente, efeitos/versão/ledger/outbox antes/depois e inspeção SQL | PASS |
| Divergências na resposta/log/métrica, sem autocorreção | Reconcile + log + Divergence counter | HTTP completo e observabilidade | PASS |
| GET health/live público sem dependências; ready PG/SQS limitada/503 | Checker sob contexto e timeout, erro público genérico | `TestHealth`, `TestReadinessPropagatesTimeout`, lifecycle/Compose reais | PASS |
| Logs JSON com IDs disponíveis sem credenciais/payload completo | slog; HTTP/consumer/outbox/reference metadados | Inspeção dos calls e teste de logs/correlação | PASS |
| Métricas status, duplicatas, retries, DLQ, conflitos, lag, latência e divergência | Metrics/sampler/endpoint; labels limitados sem IDs | `TestMetricsConcurrencyAndBoundedLabels`, HTTP + fila DLQ real | PASS |
| Métricas não bloqueiam conclusão/ack por cliente lento; sampler respeita lifecycle | Renderização em memória antes de HTTP; Stop cancela e join | `TestSlowMetricsClientDoesNotBlockOperations`, `TestMetricsSamplerShutdownJoinsAfterDeadline` | PASS |
| Segurança de transporte: body limitado, timeouts, erro sanitizado, SQL parametrizado | MaxBytesReader 64KiB, decoder estrito, HTTP timeouts/pgx placeholders | Testes de parsing/erro + inspeção SQL; concatenação só de constantes e schema de teste sanitizado | PASS |

JWKS é cacheado/atualizado pelo RemoteKeySet do go-oidc; token fabricado com kid
desconhecido é rejeitado consultando o IdP real. Não foi provocada uma rotação de
chaves administrativa no Keycloak nesta auditoria; não se afirma que esse cenário
específico tenha sido executado. Readiness verifica PG/SQS, como exige o enunciado;
OIDC é validado no startup e nas autenticações, sem requisição de token a cada probe.
GET /metrics é endpoint operacional adicional, público somente no ambiente local.

### Migrations, testes e entrega

| Requisito | Implementação | Evidência | Status |
| --- | --- | --- | --- |
| Migrations ordenadas/UP/DOWN/documentadas, checksums e atomicidade | Runner embed pgx + schema_migrations + lock administrativo | `TestPostgresMigrationUpDownUp`, backfill de referência, upgrade de outbox | PASS |
| Não editar migrations consolidadas; invariantes verificadas em SQL | 0001–0005 intactas; nova 0006 | Comparação dos arquivos; testes de SQL bypass e proteção de wallet/ledger/reversão/inbox/outbox | PASS |
| Upgrade 0006 preserva snapshots/histórico e falha se inbox legada ambígua | Constraint única, transação do runner | `TestAuditMigrationPreservesHistoryAndRejectsLegacyDuplicateInbox` | PASS |
| Unitários Money/carteira/transições/tipos/zero/opening/hash/conflito | Suítes domain/application/transport/config/security | go test e go test -race completos | PASS |
| Integração PG/SQS/IdP real, atomicidade/reentrega/retry/DLQ/recovery/Fx | Fixtures/clients reais, falhas controladas nos limites do processamento | Suíte integration completa com race | PASS |
| Restart/recovery outbox/inbox/reference sem memória anterior | Novos pools/graphs, processos independentes, leases persistidos | Crash/restart tests e API Compose após SIGTERM | PASS |
| Documentação de chamadas autenticadas, env, migrations, testes/integração/múltiplas instâncias/falhas | README curto + DEVELOPMENT/HTTP/AUTH/INBOX/OUTBOX/ARCHITECTURE | Fluxo de checkout novo e smoke HTTP autenticado | PASS |
| Compose do zero, filas/IdP automáticos, application pronta após migration explícita | compose.yaml + overlay de auditoria isolada | down -v, rebuild, health, migrations e API reais descritos abaixo | PASS |
| gofmt, go test/race/vet e diff check | Código formatado e comandos documentados | Gates F e revisão final do Git | PASS |

`context.Background()` de produção foi analisado: inicia o lifetime dos workers/
JWKS no lifecycle, dá orçamento ao CLI e limita cleanup de rollback/liberação de
visibility quando o contexto original morreu. Não substitui o contexto financeiro
de requests. O mutex de cache OAuth não protege saldo; o lock administrativo de
migrations também não participa do processamento financeiro.

## B) Problemas encontrados antes das correções

Não foi identificado problema CRITICAL confirmado. Problemas reais:

| ID | Severidade | Problema / efeito anterior |
| --- | --- | --- |
| B1 | HIGH | Trigger da inbox só aceitava REFUND/ROLLBACK pending: WIN referenciada ausente via SQS falhava no commit com SQLSTATE 23514. |
| B2 | HIGH | WIN referenciada não tinha proteção SQL de contexto/tipo/status: bypass do serviço confirmou crédito com referência incompatível em seis cenários. |
| B3 | HIGH | PK consumer/source/message ampliava a identidade do README; mudar source permitia aceitar outra operação com o mesmo messageId/consumer. |
| B4 | HIGH | Mutex de métricas permanecia adquirido durante escrita HTTP; leitor lento bloqueava contadores usados no retorno de operações/ack. |
| B5 | MEDIUM | CLI migrate carregava config completa da API: quick start somente DATABASE_URL falhava por settings HTTP/OIDC/mensageria. |
| B6 | MEDIUM | Shutdown deixava mensagens interrompidas/não iniciadas aguardando visibility original, sem liberar o trabalho explicitamente. A revisão do novo cleanup também corrigiu dois orçamentos separados para a mensagem interrompida e o resto do lote. |
| B7 | MEDIUM | Sampler podia retornar no deadline antes de observar o fim da goroutine; dependências podiam fechar antes do componente terminar. |
| B8 | MEDIUM | Cobertura HTTP/SQS era sequencial; faltava prova de disputa simultânea exigida pelo README. |
| B9 | LOW | Documentos anteriores tinham comandos de integração sem auth, variáveis dispersas e frases sobre funcionalidades já implementadas. |
| B10 | HIGH | Enforcement das policies do broker não comprovado no LocalStack Community; OIDC do consumer não autoriza produtores no broker. **Permanece pendente.** |

B1–B5 falharam em regressões antes do ajuste. B6/B7 foram confirmados por inspeção
dos caminhos de Stop e receberam testes unitários/real de comportamento. A tentativa
de executar a regressão B6 pelo Go do host foi recusada pela aprovação; não é contada
como teste executado. B8 é lacuna de evidência, corrigida com integração concorrente.
Durante a primeira rodada completa houve também erro na nova fixture de duplicata
legada: clock_timestamp produziu processed_at anterior ao default received_at. A
fixture passou a fornecer ambos com o mesmo now(), preservando a constraint.

## C) Correções realizadas

| Problema | Arquivos | Mudança | Teste |
| --- | --- | --- | --- |
| B1/B2 | `migrations/0006_audit_hardening.up.sql/.down.sql`, runner | WIN pending permitida na inbox; WIN PROCESSED com referência exige BET/contexto válido. Payout diferente permanece permitido. | `TestReferencedWinSQSCommitsPendingAndRecovers`, `TestDatabaseRejectsInvalidReferencedWin` |
| B3 | Migration 0006, `platform/postgres/inbox.go` | UNIQUE consumer/message; Get por essa identidade e confronto de source; upgrade não apaga ambiguidades. | `TestInboxMessageIdentityCannotChangeSource`, teste de migration/histórico |
| B4 | `platform/observability/metrics.go` | Renderiza snapshot limitado em memória sob mutex, libera antes de escrever HTTP; copia timestamp de lag. | `TestSlowMetricsClientDoesNotBlockOperations`, `TestOutboxMetricOwnsTimestampSnapshot` |
| B5 | `config/config.go`, `cmd/migrate/main.go` | Validação compartilhada de DATABASE_URL; CLI só lê essa configuração. Loader da API continua estrito. | `TestMigrateOnlyRequiresDatabaseConfiguration`, migrate real no banco novo sem env OAuth |
| B6 | `workers/consumer.go`, `messaging/sqs.go`, policy consumer | ChangeMessageVisibility=0 para trabalho interrompido/não iniciado; único orçamento de cleanup para o lote antes de fechar SQS. | `TestConsumerShutdownReleasesInterruptedMessage`, `TestConsumerShutdownReleasesUnstartedBatch`, `TestConsumerShutdownSharesCleanupBudget`, `TestConsumerShutdownReleasesRealSQSVisibility` |
| B7 | `workers/metrics.go` | Stop cancela e aguarda done mesmo se deadline já venceu. | `TestMetricsSamplerShutdownJoinsAfterDeadline` |
| B8 | `application/hardening_integration_test.go` | 50 requests HTTP autorizados e duas entregas SQS reais competem em três pools independentes. | `TestConcurrentAuthenticatedHTTPAndSQSUseOneFinancialCommit` |
| B9 | README, ARCHITECTURE, docs e migrations README | Quick start reparado, todas as settings documentadas, auth nos comandos, enunciado original preservado separadamente. | Execução real do zero, API autenticada, gates completos |

O teste de backfill da 0005 agora reverte também a 0006 antes de simular a versão
antiga; não houve alteração dos SQLs 0001–0005. A API e regras financeiras foram
preservadas, incluindo BRL único, política de reversões e saldo inicial zero.

## D) Legibilidade

O handler de operação reutiliza decodeBody: continuam um único JSON, campos
conhecidos, limite 64KiB e HTTP 400 nos mesmos casos. prepareReversalReference foi
renomeado prepareFinancialReference porque valida WIN e reversões; apenas nome e
callsites mudaram. A mensagem interna de ErrUnsupportedOperation agora descreve
tipo externo não suportado; o sentinela/contrato HTTP continuam iguais. Comentários
que anunciavam implementação futura foram atualizados. Não houve reescrita das
regras, loops financeiros, ordem dos locks ou transações para fins estéticos.

## E) Critérios eliminatórios

| Critério do enunciado | Resultado | Justificativa |
| --- | --- | --- |
| Ausência de autenticação efetiva em endpoints de negócio | PASS | Middleware + wrapper com OIDC real; 401/403 e SQL sem efeitos nos testes. |
| Acesso não autorizado a operações ou transações | **FAIL na fronteira do broker local** | API/consultas/consumer passaram isolamento, mas broker IAM não foi comprovado. Um produtor com acesso local pode usar um provider permitido pelo consumer. |
| Cálculo monetário em ponto flutuante | PASS | Nenhum float/ParseFloat/FormatFloat no código próprio; centavos BIGINT/int64. |
| Saldo negativo por concorrência | PASS | Disputa de 80/80 preserva 20; domínio e CHECK/locks por wallet. |
| Movimentação duplicada | PASS | 50 paralelas, três processos, disputa HTTP/SQS e reentrega real: um débito. |
| Idempotência restrita à memória | PASS | Identidades/hash/result/inbox no PostgreSQL; nova instância e restart real. |
| Dependência de uma única instância | PASS | Três processos independentes e workers com claim/lease persistidos. |
| Publicação anterior ao commit | PASS | Worker separado; evento não confirmado não fica elegível. |
| Ausência de ledger auditável | PASS | Append-only SQL, equação e vínculo com versão/operação; reconciliação. |
| PostgreSQL/SQS/IdP integralmente substituídos por mocks | PASS | Três containers reais em integração; mocks só para testes unitários de limites. |

FAIL aqui não afirma uma invasão da API OIDC: o requisito de controle de acesso
à mensageria faz parte da fronteira obrigatória, e sua validação local não pode
ser promovida a PASS. Essa é a avaliação conservadora da pendência eliminatória.

## F) Testes e comandos

Gates executados em `golang:1.26.0-bookworm`, Linux/GCC/CGO, rede `jungle-audit_default`:

```sh
go test ./...
go test -race ./...
go vet ./...
go test -race -tags=integration -count=1 ./...
```

Todos terminaram com exit code 0; go vet sem diagnósticos. Integração financeira
com race passou em 51.386s na rodada completa contra infraestrutura nova. Não
houve skip de dependência externa. Wrapper/env reproduzíveis estão em
[DEVELOPMENT.md](DEVELOPMENT.md#infraestrutura-isolada-para-auditoria).

Antes das correções, foram executados os seletores:

```sh
go test ./cmd/migrate ./internal/platform/observability -run 'TestMigrateOnlyRequiresDatabaseConfiguration|TestSlowMetricsClientDoesNotBlockOperations' -count=1
go test -race -tags=integration -count=1 -run 'TestReferencedWinSQSCommitsPendingAndRecovers|TestInboxMessageIdentityCannotChangeSource|TestDatabaseRejectsInvalidReferencedWin' -v ./internal/application
```

Ambos falharam pelo problema que reproduzem. A primeira tentativa do Go do host
também teve cache bloqueado pelo sandbox; a execução autorizada confirmou as duas
regressões. O primeiro gate completo pós-ajuste falhou na fixture temporal descrita
em B; após corrigi-la a suíte completa passou. Buscas e verificações finais:

```sh
gofmt -l internal cmd migrations/migrate.go
git diff --check
rg -n 'float32|float64|ParseFloat|FormatFloat|TODO|FIXME|HACK|panic\(|placeholder' internal cmd migrations --glob '*.go' --glob '*.sql'
git status --short
```

Nenhuma ocorrência no código/SQL próprio e nenhuma pendência de gofmt/diff. A busca
global na documentação encontra os nomes dos tipos proibidos e os comandos de
auditoria no enunciado/relatório, não representações financeiras ou trabalho TODO.
A execução do host de B6 recusada não substituiu a execução Linux posterior, na
qual os testes de shutdown foram efetivamente executados com race.

## G) Infraestrutura do zero

Foi criado e conferido o volume `jungle-audit_postgres-data`, com label de projeto
`jungle-audit`. Em seguida foi removido **somente esse projeto**, com down -v,
e reconstruído com volume PostgreSQL novo. O projeto habitual foi preservado.
Comandos executados na raiz da cópia de trabalho auditada:

```sh
docker compose -p jungle-audit -f compose.yaml -f compose.audit.yaml config --quiet
docker compose -p jungle-audit -f compose.yaml -f compose.audit.yaml down -v
docker compose -p jungle-audit -f compose.yaml -f compose.audit.yaml up -d --build --wait postgres localstack keycloak
go run ./cmd/migrate up
docker compose -p jungle-audit -f compose.yaml -f compose.audit.yaml up -d --build --wait api
docker compose -p jungle-audit -f compose.yaml -f compose.audit.yaml stop -t 20 api
docker compose -p jungle-audit -f compose.yaml -f compose.audit.yaml ps -a --format json api
docker compose -p jungle-audit -f compose.yaml -f compose.audit.yaml start --wait api
```

Migrate foi executado no container Go da rede audit, com **somente DATABASE_URL**;
UP aplicou 0001–0006 sem IdP/SQS/env de API. Keycloak importou realms/service accounts;
LocalStack criou input/events/DLQ/redrive; todos os health checks ficaram saudáveis.
API construída pelo Dockerfile real: readiness PostgreSQL/SQS ok, metrics 200,
wallet interna 100.00, BET autorizada 25.00 → 75.00, replay sem débito adicional,
reconciliation consistente/difference 0.00/duas entradas. SIGTERM terminou com
ExitCode=0; após start o mesmo request retornou idempotentReplay=true, saldo
original 75.00 e walletVersion=2. O ambiente novo também executou toda a integração.

## H) Limitações conhecidas

LocalStack Community não comprovou enforcement IAM; policies DEV são templates,
não prova de AccessDenied no broker. A autorização OIDC do consumer é real e
testada, mas não substitui essa fronteira. Keycloak start-dev, secrets locais e
usuário PostgreSQL com DDL são somente desenvolvimento; produção exige usuário
DML separado, credenciais adequadas e IdP/broker devidamente protegidos.

Publicação é at-least-once: crash após envio pode republicar o mesmo eventId;
consumidores de saída precisam deduplicar por eventId além da janela FIFO.
Metrics são por processo, DLQ é estoque visível aproximado e lag é amostrado.
Falha do broker ao liberar visibility no shutdown mantém o timeout original;
inbox/idempotência preservam segurança na reentrega. Rotação administrativa real
de chave do Keycloak não foi induzida, conforme a nota da matriz.

UP da 0006 exige ausência de duplicatas legadas consumer/message entre sources;
se houver ambiguidades, ele falha atomicamente, preservando todos os registros.
DOWN do runner remove todo o schema financeiro/dados e só foi testado em schema
temporário. Não há tracing, dashboards, carga, partidas dobradas ou outros extras.

## I) Pendências obrigatórias

**Acesso à mensageria controlado por credenciais/policies aplicadas no broker.**
Motivo: o ambiente Community disponível não comprova IAM. Impacto: autorização
do produtor local não demonstrada, inclusive quando ele publica payload de um
provider que o consumer permite. Próxima evidência necessária: em AWS ou ambiente
com enforcement, aplicar as policies a identidades distintas e executar SendMessage
autorizado, AccessDenied para identidade sem permissão, consumo/publicação e
liberação de visibility com a identidade do serviço. Não foi ativado serviço pago
nem declarado sucesso de AWS IAM sem executar esse teste.

## J) Veredito

**NOT READY**

As correções funcionais e gates locais foram concluídos. O requisito obrigatório
de controle efetivo do broker permanece pendente; portanto o resultado não pode
ser READY FOR FINAL REVIEW. Nenhum commit ou push foi realizado.
