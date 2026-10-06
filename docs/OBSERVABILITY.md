# Observabilidade básica — Parte 6B

Logs JSON usam slog e IDs disponíveis de correlação, mensagem/envelope, provider,
wallet, transação, ID externo e evento. HTTP registra resultado/status e latência
em microssegundos. Consumer inclui a identidade de correlação e tempo de resolução;
publisher registra metadados projetados do snapshot, nunca seu payload completo;
reference worker registra tentativa/latência e transactionId. Nenhum JWT,
Authorization, client secret ou credencial AWS é incluído nos logs.

GET /metrics é público no Compose local. Fora desse ambiente, restrinja sua rede
ou gateway operacional. Não contém saldos, players, payloads, tokens ou IDs de
recursos. Não foi adicionada stack de Prometheus/Grafana. A exposição segue o
[formato texto Prometheus 0.0.4](https://prometheus.io/docs/instrumenting/exposition_formats/),
com contadores e histogramas em inteiros/microssegundos, convertidos para segundos
somente na representação textual, sem cálculo monetário ou ponto flutuante.

| Métrica | Semântica / labels |
| --- | --- |
| jungle_operations_total | Invocações do processamento compartilhado, incluindo replays; transport, kind e result |
| jungle_replays_total | Replay financeiro ou inbox; transport |
| jungle_worker_results_total | Resultados observados pelo worker; worker e result |
| jungle_retries_total | Falhas deixadas para redelivery, envio com retry persistido ou referência aguardando nova tentativa; worker |
| jungle_concurrency_conflicts_total | Erros de versão, serialização (40001) ou deadlock (40P01) observados, sem contar cada espera normal de lock |
| jungle_http_requests_total | Requests nas rotas privadas, incluindo erros de auth; classe de status result |
| jungle_http_duration_seconds | Histograma das rotas privadas, incluindo validação/auth/financeiro; sem IDs |
| jungle_reconciliation_divergences_total | Cada auditoria que reporta consistent=false, incluindo auditorias repetidas da mesma divergência |
| jungle_outbox_lag_seconds | Idade do evento não publicado mais antigo no último sample bem-sucedido |
| jungle_dlq_visible_messages | Quantidade aproximada de mensagens visíveis na DLQ da redrive policy de entrada |
| jungle_metrics_dependency_available | Sucesso do último sample de postgres/sqs |

Labels têm vocabulários limitados; kinds/status desconhecidos são normalizados.
Nenhum walletId, playerId, providerId, transactionId, externalTransactionId,
idempotencyKey ou correlationId é label. Contadores são por processo e reiniciam
com ele; não representam um inventário financeiro persistente. Replays contam
invocações bem-sucedidas adicionais, sem indicar novas movimentações. OPENING usa
o caso de uso interno e é observável por HTTP/logs/ledger/eventos; o contador de
operações financeiras cobre Process, ProcessIncoming e resolução de referências.

## Coleta de infraestrutura

MetricsSampler usa lifecycle Fx, coleta na inicialização e a cada 15 segundos,
com contexto cancelável e timeout por dependência. Uma falha de coleta não impede
startup; mantém o último valor e marca dependency_available=0. Antes do primeiro
sucesso, valores são zero e disponibilidade zero. Shutdown cancela e aguarda a
goroutine antes de fechar suas dependências. Scrape HTTP só lê memória.

Outbox consulta MIN(occurred_at) dos eventos unpublished, incluindo eventos em
backoff ou com lease. Sem pendências, lag=0. Entre samples, a idade cresce a partir
do timestamp guardado; a remoção da última pendência pode levar até um intervalo
para aparecer. Não há consulta ao PostgreSQL a cada request/scrape.

DLQ consulta GetQueueAttributes/ApproximateNumberOfMessages da fila indicada em
RedrivePolicy. Mede estoque visível aproximado, não número acumulado de mensagens
enviadas, mensagens in-flight ou confirmação de esgotamento de cada mensagem.
Essa distinção segue as [métricas do SQS](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-available-cloudwatch-metrics.html).
A policy do serviço inclui GetQueueUrl/GetQueueAttributes nessa DLQ; não permite
consumi-la/apagar suas mensagens. A limitação IAM local permanece em [AUTH.md](AUTH.md).

Readiness cobre PostgreSQL e SQS conforme README, sem publicação/probes pesados.
Liveness é independente dessas dependências. Correlation ID não é idempotência;
ausente no HTTP, é gerado, e está presente na resposta/logs/eventos.

## Verificação

Unitários cobrem atualização concorrente dos contadores, labels limitados,
histograma, formato, propagação de correlação e ausência de credenciais nos logs.
Integrações reais verificam operação/replay/divergência na exposição, o evento
pendente no PostgreSQL e uma mensagem conhecida visível na DLQ real. Os testes de
regressão continuam exercitando retry, publicação concorrente e redrive efetivo.
Use os comandos de [DEVELOPMENT.md](DEVELOPMENT.md) e [AUTH.md](AUTH.md), incluindo:

```sh
go test ./...
go test -race ./...
go vet ./...
go test -race -tags=integration -count=1 ./...
```
