# OAuth2/OIDC — Parte 6A

Keycloak externo emite access tokens pelo fluxo `client_credentials`; a aplicação
não cadastra senhas nem emite tokens. A configuração é importada automaticamente
de `docker/keycloak/*-realm.json`, usando o mecanismo oficial de
[realm import](https://www.keycloak.org/server/importExport).
Compose espera o healthcheck do Keycloak antes de iniciar a API.

## Realm e identidades locais

Realm operacional: **jungle**. Issuer: `http://localhost:8081/realms/jungle`.
Discovery: `<issuer>/.well-known/openid-configuration`.
Token endpoint: `<issuer>/protocol/openid-connect/token`.
JWKS: `<issuer>/protocol/openid-connect/certs`.

| Client | Secret exclusivamente DEV | Role / identidade |
| --- | --- | --- |
| provider-alpha | provider-alpha-local | wagering-provider; provider_id=provider-alpha |
| provider-beta | provider-beta-local | wagering-provider; provider_id=provider-beta |
| provider-a | provider-a-local | wagering-provider; compatibilidade com exemplos anteriores |
| wagering-messaging | wagering-messaging-local | wagering-messaging; provider_ids=[provider-alpha, provider-beta, provider-a] |
| wallet-internal | wallet-internal-local | wallet-internal; reservado às futuras operações internas de carteira |

Clients são confidenciais, com service accounts; login humano e password grant
estão desabilitados. O realm de testes `jungle-other` fornece um token de issuer
diferente. Clients `auth-test-missing-provider`, `auth-test-no-role` e
`auth-test-wrong-audience` testam claims/permissões inadequadas; seus secrets DEV
seguem `<clientId>-local`. Esses clients/realm auxiliares pertencem somente ao
ambiente de teste. Nenhuma credencial publicada aqui é adequada para produção.

## Validação e autorização

`go-oidc` valida assinatura RS256 usando JWKS, issuer exato, expiração e audience
`jungle-api`. A aplicação exige `sub`, `azp`, `typ=Bearer`, respeita `nbf` e exige
`provider_id` quando o token possui role de provider. Tokens ID e alg=none não
são aceitos. Claims só são usadas depois da verificação criptográfica.

Um `security.Principal` é colocado no contexto. O domínio permanece independente
de JWT/Keycloak/Fx. `provider_id` assinado é a fonte autoritativa; `providerId` no
JSON precisa ser igual. `AuthorizedFinancialService` repete essa verificação
antes de chamar o caso de uso interno e filtra consultas pelo provider autenticado.
Idempotência permanece isolada por provider, inclusive com chaves/IDs externos iguais.

Sem Bearer, header malformado, assinatura inválida, token expirado, issuer/audience
inválidos ou claim obrigatória ausente: **401**, JSON `{"error":"unauthorized"}`
e WWW-Authenticate. Token autenticado sem role adequada ou provider divergente:
**403**, JSON `{"error":"forbidden"}`. Nenhum erro revela token ou detalhes do IdP.

O RemoteKeySet de [go-oidc](https://github.com/coreos/go-oidc/blob/v3/oidc/jwks.go)
mantém chaves em cache e atualiza o JWKS quando as chaves em cache não verificam
uma assinatura, permitindo rotação. Não há busca por request válida nem chave
pública hardcoded. HTTP do IdP usa timeout e recursos encerrados pelo lifecycle Fx.

## Rotas e escopo

`GET /health/live` e `GET /health/ready` continuam públicos.
Foi acrescentado apenas `POST /wagering/transactions`, para demonstrar a proteção
com processamento financeiro real. Exige Bearer, role wagering-provider e header
Idempotency-Key; aceita o corpo já definido no enunciado. Money continua string
decimal com duas casas, somente BRL. Retorna 200 processado/replay, 202 pending,
422 rejeição de negócio, 400 entrada inválida, 409 conflito ou 503 indisponibilidade.
O resultado contém transactionId, status, failureCode quando aplicável,
idempotentReplay e saldo/versão originais quando presentes.

Criação/consulta de carteiras, ledger paginado, consultas HTTP de transações e
reconciliation ainda pertencem à Parte 6B. Abertura permanece um caso de uso
interno, sem rota pública; credencial de provider não recebe essa permissão.

## Mensageria e fronteira de confiança

O README exige **credenciais/políticas do broker**, não JWT no corpo da mensagem.
AWS SDK mantém assinatura SigV4 e a cadeia padrão de credenciais AWS. Policies
mínimas estão em `docker/aws/consumer-policy.json` e `producer-policy.json`:
aplique-as a roles distintas, substituindo conta/região/ARN conforme a instalação.
O processo consumer/publisher recebe a primeira; produtores confiáveis recebem
somente SendMessage na fila de entrada. Providers HTTP não recebem credencial AWS.
Use roles/credenciais temporárias fora do ambiente local; não confunda secret
OAuth com access key AWS.

Além do broker, o consumer autentica sua service account **wagering-messaging**
via client_credentials no startup e antes de ProcessIncoming, com role específica
e providers autorizados no claim assinado `provider_ids`. O consumer é um serviço
confiável que processa operações dos providers desse conjunto, sem impersonar
um provider HTTP. Mensagens de outro provider são negadas antes de inbox/financeiro
e permanecem para redelivery/DLQ; uma autorização negada não gera ledger/outbox.

O token OAuth do serviço é verificado com o mesmo JWT verifier, cacheado apenas
em memória até 10 segundos antes da menor expiração declarada pelo OAuth/JWT e
renovado sob context/timeout. Não se busca token por mensagem. Nenhum token ou
client secret é incluído em payload, PostgreSQL, domínio, resposta ou log.

Fluxo: credencial AWS + policy → SQS → consumer autenticado por OIDC → allowlist
de provider → serviço financeiro compartilhado → commit inbox/ledger/outbox → ack.

**Limitação do ambiente local:** LocalStack Community 4.2 usa credenciais DEV
`test` e não comprova enforcement de IAM. O recurso ENFORCE_IAM é indicado como
[Pro na documentação](https://docs.localstack.cloud/aws/customization/configuration-options/).
As policies versionadas são para aplicar em AWS/ambiente com enforcement; não
alegamos que o broker local restringe produtores por IAM. A autorização OIDC do
consumer e a rejeição de providers fora da allowlist são testadas de verdade.

## Configuração e execução

Obrigatórios: OIDC_ISSUER_URL, OIDC_AUDIENCE, MESSAGING_CLIENT_ID e
MESSAGING_CLIENT_SECRET. São validados pelo loader; falha de discovery ou de
autenticação da mensageria impede startup. `.env.example` contém valores DEV.

No host, deixe OIDC_INTERNAL_URL vazio. Dentro de Docker, use
`http://keycloak:8080`: somente o transporte para a origem configurada do IdP
é redirecionado, mantendo path e issuer público validado. Tokens emitidos via
host e via rede Docker têm o mesmo issuer. Não desabilitamos validação de issuer.

```sh
docker compose up -d --wait postgres localstack keycloak
# Configure DATABASE_URL e aplique as migrations existentes antes da API.
go run ./cmd/migrate up
docker compose up -d --build api

curl -sS -X POST http://localhost:8081/realms/jungle/protocol/openid-connect/token \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode grant_type=client_credentials \
  --data-urlencode client_id=provider-alpha \
  --data-urlencode client_secret=provider-alpha-local
```

Use access_token da resposta como Bearer. Com wallet existente criada pelo caso
de uso interno, uma requisição reproduzível é:

```sh
curl -i -X POST http://localhost:8080/wagering/transactions \
  -H "Authorization: Bearer $ACCESS_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: demo-bet-1' \
  -d '{"providerId":"provider-alpha","externalTransactionId":"demo-bet-1","playerId":"PLAYER_ID","walletId":"WALLET_ID","roundId":"round-1","gameId":"game-1","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}'
```

Trocar somente providerId por provider-beta com o token de alpha retorna 403,
antes de qualquer efeito financeiro. Não copie tokens/secrets para logs ou commits.

## Testes

Execute go test ./..., go test -race ./... e go vet ./.... Integrações exigem
PostgreSQL, LocalStack e Keycloak reais. No host configure as variáveis AUTH acima,
TEST_DATABASE_URL, TEST_SQS_ENDPOINT e credenciais AWS DEV, e execute:

```sh
go test -race -tags=integration -count=1 ./...
```

Em Linux Docker, mantenha issuer público e configure OIDC_INTERNAL_URL para
Keycloak na rede do Compose. Use o container Go/GCC documentado em OUTBOX.md,
acrescentando essas cinco variáveis de autenticação. Testes reais cobrem tokens
de alpha/beta, issuer distinto, audience/claims inválidos, JWT fabricado/alg=none,
401/403, health público, BET autenticada, ausência de efeitos na autorização
negada, consultas/replays isolados, identidade SQS, cache/renovação e regressões.
Expiração é testada com um JWT real originalmente válido e relógio do verifier
avançado além do exp, sem alterar claims/assinatura nem esperar minutos. Também
se testa startup negado com secret inválido ou client de provider usado como serviço.
