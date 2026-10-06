# API HTTP — Parte 6B

Todas as rotas de negócio exigem access token real do Keycloak. Consulte
[AUTH.md](AUTH.md) para clients, claims e validação JWT/JWKS. Providers acessam
somente suas próprias transações; **todas as operações de carteira são restritas
à role wallet-internal**, conforme o README original. Wallet não possui provider:
é única globalmente por `(playerId, currency)`. Não foi criada uma associação
artificial de ownership por provider. O exemplo do prompt em que um provider cria
wallet foi substituído pela credencial interna para respeitar essa exigência.

| Método / rota | Permissão | Resultado |
| --- | --- | --- |
| POST /wallets | wallet-internal | Criação com initialBalance; 201 e Location |
| GET /wallets/{walletId} | wallet-internal | Estado atual, versão e timestamps |
| GET /wallets/{walletId}/ledger?cursor=...&limit=50 | wallet-internal | Histórico paginado |
| POST /wallets/{walletId}/reconciliation | wallet-internal | Auditoria de leitura |
| POST /wagering/transactions | wagering-provider | Processamento / replay / pendência |
| GET /wagering/transactions/{transactionId} | wagering-provider | Snapshot persistido do provider |
| GET /providers/{providerId}/wagering/transactions/{externalTransactionId} | wagering-provider | Consulta por identidade externa |
| GET /health/live | Público | Processo vivo |
| GET /health/ready | Público | PostgreSQL e SQS disponíveis |
| GET /metrics | Público no ambiente local | Exposição operacional sem IDs / secrets |

O provider do path/body deve coincidir com o claim assinado: divergência retorna
403 antes de ler/processar. Consulta por ID interno de outro provider retorna 404,
igual a um recurso inexistente. Tokens de provider não acessam carteira, ledger ou
reconciliation, mesmo que esse provider já tenha operado na carteira. A service
account de mensageria não recebe permissão de provider HTTP nem de wallet interna.
Não há endpoint de consulta por player/currency, pois o README não o exige.

## Abertura e dinheiro

Contrato: `{"playerId":"player","initialBalance":{"amount":"100.00","currency":"BRL"}}`.
Resposta: `id`, `playerId`, `balance`, `version`, `createdAt` e `updatedAt`.
Valores monetários usam o Money existente, strings com exatamente duas casas e
BRL; números JSON, valores negativos, NaN/Infinity e notação científica são inválidos.

Saldo inicial zero cria somente a wallet, com versão 1. Saldo positivo cria wallet
na versão 1, OPENING PROCESSED, ledger CREDIT e os eventos WagerTransactionProcessed
e WalletBalanceChanged no mesmo commit PostgreSQL. Uma falha em qualquer escrita
reverte tudo. Unicidade `(player_id,currency)` e `one_opening_per_wallet` já existem
nas migrations anteriores; nenhuma migration nova foi necessária. OPENING externo
é rejeitado tanto por HTTP quanto por SQS, sem movimentação financeira.

## Processamento e consultas

Idempotency-Key é obrigatório e permanece exatamente o recebido; não é gerado a
partir do payload nem substituído pelo correlation ID. O hash canônico e o caso de
uso são compartilhados com SQS. BET/WIN/LOSS/REFUND/ROLLBACK mantêm suas regras.
WIN aceita referência opcional a uma BET processada no mesmo contexto; o payout
pode diferir do valor apostado. Referência ausente usa PENDING_REFERENCE e o worker
durável existente; referência encontrada sem sucesso/contexto inválido é rejeitada.
A referência é consultada pelo provider autenticado e ID externo.

A submissão retorna transactionId, status, failureCode quando aplicável,
idempotentReplay e balance/walletVersion originais quando registrados. Replay não
consulta o saldo atual para substituir esse resultado. As consultas retornam também
externalTransactionId, providerId, walletId/playerId, roundId/gameId, kind, money,
referências resolvidas/externas e timestamps. OPENING não possui provider externo
e não fica acessível por uma credencial de provider.

Ledger retorna `{"items":[...],"nextCursor":"..."}`; cada item possui id, walletId,
transactionId, direction, money, balanceBefore, balanceAfter e createdAt.
O cursor é opaco, vinculado à wallet, e codifica `(created_at,id)` em base64 URL.
Ordem crescente estável; limite padrão 50, mínimo 1 e máximo 200. A query busca
apenas limit+1 registros usando o índice existente. nextCursor é omitido na última
página. Cursor malformado/de outra carteira e limite inválido retornam 400.
Paginação de histórico em crescimento não congela um snapshot entre requests;
cada página respeita a mesma ordem e fronteira. Ledger não oferece edição/exclusão.

## Reconciliação e health

Reconciliation reconstrói créditos menos débitos desde zero, incluindo OPENING,
e verifica balanceBefore/balanceAfter de cada lançamento. A leitura é streaming,
ordenada pela versão financeira; wallet e ledger compartilham uma transação
REPEATABLE READ READ ONLY, evitando falsa divergência durante commits concorrentes.
Resposta: walletId, storedBalance, calculatedBalance, difference (= stored minus
calculated), consistent, checkedEntries e chainConsistent. Zero sem ledger é válido.
Divergência retorna 200 com consistent=false e é registrada no log/métrica.
Não altera saldo, versão, transações, ledger ou outbox; não corrige inconsistências.
Overflow ao reconstruir dados administrativamente corrompidos retorna erro interno,
sem arredondar ou produzir um saldo monetário fora do limite int64.

Liveness não consulta dependências. Readiness verifica ping do PostgreSQL e atributos
da fila SQS, sob context/timeout; falha retorna 503. Não publica mensagens, não
executa reconciliação nem busca token por probe. OIDC é validado no startup e no
caminho de autenticação, com cache JWKS; readiness não testa o IdP em cada chamada.

## Status e erros

| Status | Uso |
| --- | --- |
| 200 | Consultas, processamento/replay bem-sucedido e reconciliation |
| 201 | Wallet criada |
| 202 | PENDING_REFERENCE, inclusive replay ainda pendente |
| 400 | JSON, Money, identidade/chave/tipo, cursor ou limite inválido |
| 401 | Token ausente/malformado/inválido/expirado |
| 403 | Role insuficiente ou provider divergente |
| 404 | Recurso inexistente no contexto autorizado |
| 409 | Wallet duplicada, identidade/idempotência conflitante ou mudança concorrente |
| 422 | REJECTED financeiro, inclusive replay da rejeição |
| 500 | Erro interno sem detalhes de implementação |
| 503 | Dependência temporariamente indisponível / timeout e readiness negativo |

Erros de transporte seguem `{"error":"invalid_input"}` / `forbidden` / `not_found`
etc.; não expõem SQL, stack ou erro do IdP. Rejeições persistidas usam a resposta
de operação com failureCode estável. Corpos JSON aceitam uma única estrutura,
campos conhecidos e até 64 KiB. Responses incluem Cache-Control: no-store.
X-Correlation-ID é propagado ao contexto, logs e eventos; ausente, é gerado pelo
servidor. Limite 128 caracteres, sem espaços nas extremidades. Não funciona como
idempotência. O I/O respeita o contexto/cancelamento do request.

## Exemplo local completo

Requer curl e jq, infraestrutura preparada e migrations aplicadas conforme
[DEVELOPMENT.md](DEVELOPMENT.md). Secrets abaixo são exclusivamente DEV.

```sh
TOKEN_URL=http://localhost:8081/realms/jungle/protocol/openid-connect/token
INTERNAL_TOKEN=$(curl -fsS -X POST "$TOKEN_URL" \
  -d grant_type=client_credentials -d client_id=wallet-internal \
  -d client_secret=wallet-internal-local | jq -r .access_token)
PROVIDER_TOKEN=$(curl -fsS -X POST "$TOKEN_URL" \
  -d grant_type=client_credentials -d client_id=provider-alpha \
  -d client_secret=provider-alpha-local | jq -r .access_token)
API=http://localhost:8080
WALLET_ID=$(curl -fsS -X POST "$API/wallets" \
  -H "Authorization: Bearer $INTERNAL_TOKEN" -H 'Content-Type: application/json' \
  -d '{"playerId":"demo-player","initialBalance":{"amount":"100.00","currency":"BRL"}}' | jq -r .id)
curl -fsS "$API/wallets/$WALLET_ID" -H "Authorization: Bearer $INTERNAL_TOKEN"
TX_ID=$(curl -fsS -X POST "$API/wagering/transactions" \
  -H "Authorization: Bearer $PROVIDER_TOKEN" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: demo-bet' -H 'X-Correlation-ID: demo-request' \
  -d "{\"providerId\":\"provider-alpha\",\"externalTransactionId\":\"demo-bet\",\"playerId\":\"demo-player\",\"walletId\":\"$WALLET_ID\",\"roundId\":\"round-1\",\"gameId\":\"game-1\",\"kind\":\"BET\",\"money\":{\"amount\":\"25.00\",\"currency\":\"BRL\"}}" | jq -r .transactionId)
curl -fsS "$API/wagering/transactions/$TX_ID" -H "Authorization: Bearer $PROVIDER_TOKEN"
curl -fsS "$API/providers/provider-alpha/wagering/transactions/demo-bet" -H "Authorization: Bearer $PROVIDER_TOKEN"
curl -fsS "$API/wallets/$WALLET_ID/ledger?limit=50" -H "Authorization: Bearer $INTERNAL_TOKEN"
curl -fsS -X POST "$API/wallets/$WALLET_ID/reconciliation" -H "Authorization: Bearer $INTERNAL_TOKEN"
curl -fsS "$API/health/live"
curl -fsS "$API/health/ready"
curl -fsS "$API/metrics"
```

Repetir a abertura de demo-player retorna 409; para outro cenário use outro player.
Repetir a mesma BET/chave retorna o resultado original sem novo débito.
