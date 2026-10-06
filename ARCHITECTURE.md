# Arquitetura — Partes 1, 2, 3, 4A, 4B, 5A, 5B e 6A

Esta entrega estrutura o serviço e implementa as invariantes locais do núcleo
financeiro. A Parte 3 implementa persistência financeira, processamento síncrono,
locks por carteira, idempotência persistente e registro atômico da outbox.
A Parte 4A acrescenta publicação SQS FIFO com recuperação por lease e retry.
A Parte 4B consome SQS usando o mesmo fluxo financeiro, com inbox no mesmo commit.
A Parte 5A implementa REFUND integral e persiste referência pendente com evento.
A Parte 5B implementa ROLLBACK e resolução persistente, com claim/lease, backoff
e limite de tentativas. A política de reversão única por BET, ordem dos locks e
recuperação estão em [REFERENCE_RETRY.md](docs/REFERENCE_RETRY.md).
Veja [docs/DOMAIN.md](docs/DOMAIN.md) para o domínio e
[docs/POSTGRES.md](docs/POSTGRES.md) para transações, migrations e testes reais.

## Organização

| Pacote | Responsabilidade |
| --- | --- |
| `cmd/api` | Entrypoint, execução da aplicação Fx |
| `internal/app` | Composição dos módulos |
| `internal/config` | Leitura e validação de ambiente |
| `internal/platform/logging` | Logs JSON com `log/slog` |
| `internal/platform/postgres` | Pool pgx, lifecycle, repositories e delimitação de pgx.Tx |
| `internal/platform/messaging` | Cliente AWS SDK v2, checks SQS e envio de snapshots |
| `internal/transport/http` | Servidor net/http e health checks |
| `internal/transport/sqs` | Contrato JSON de entrada e mapping para FinancialService |
| `internal/domain` | Money, Wallet, WagerTransaction e WalletLedgerEntry; apenas biblioteca padrão |
| `internal/application` | Abertura, BET/WIN/LOSS/REFUND/ROLLBACK e resolução de referências, compartilhados por HTTP/SQS/worker |
| `internal/workers` | Publisher, consumer SQS e ReferenceWorker com polling e lifecycle Fx |
| `migrations` | Schema financeiro UP/DOWN, runner pgx e arquivos SQL embutidos |
| `cmd/migrate` | Aplicação/reversão explícita das migrations versionadas |

Fx faz a composição por construtores, `fx.Module`, `fx.Provide` e `fx.Invoke`.
O domínio não possui service locator nem dependência de Fx.
Ainda não há repositórios ou casos de uso artificiais apenas para preencher o grafo.

## Inicialização e shutdown

Os construtores validam configuração e registram hooks. O I/O de inicialização
ocorre em `OnStart`, com contexto e prazo. A ordem é PostgreSQL, SQS e HTTP,
determinada pelas dependências do handler de readiness.

O pool abre e executa ping; o cliente SQS usa a cadeia padrão de credenciais do
AWS SDK, resolve a fila e consulta seus atributos. Para LocalStack é configurado
`BaseEndpoint`, conforme a [documentação do AWS SDK](https://docs.aws.amazon.com/sdk-for-go/v2/developer-guide/configure-endpoints.html).
Falha de startup aborta o início, e Fx libera recursos já inicializados.

O servidor abre o listener sincronamente, de modo que erro de bind impede startup.
Serve em goroutine e sinaliza Fx com código de saída 1 se encerrar inesperadamente.
SIGINT/SIGTERM inicia shutdown: HTTP para de aceitar entradas e aguarda requisições;
no fim do prazo força fechamento e observa o término da goroutine. O pool é fechado
depois do servidor; o transporte HTTP do SQS também fecha suas conexões ociosas.
Essa ordem inversa dos hooks é descrita na
[documentação do Fx](https://uber-go.github.io/fx/lifecycle.html).
Startup tem orçamento global de 30s; shutdown, de 15s; Compose concede 20s ao processo.

Os workers param de buscar trabalho e aguardam suas goroutines antes do fechamento
do banco. ReferenceWorker cancela claims/resolução; escritas não confirmadas são
desfeitas e os leases permitem recuperação. Consumer remove mensagens somente
após commit durável; publisher mantém seus leases e snapshots persistidos.

## Persistência e integridade da Parte 3

Money é persistido em BIGINT com centavos exatos e currency BRL separada.
Versão e resultado financeiro usam BIGINT; timestamps usam TIMESTAMPTZ.
Nenhum dinheiro passa por ponto flutuante. As entidades são reidratadas pelas
funções de domínio, preservando versões, timestamps e resultados, sem novos efeitos.

`Store.WithTx` abre READ COMMITTED e entrega quatro repositories que compartilham
a mesma pgx.Tx. Repositories não abrem nem confirmam transações próprias.
Erro provoca rollback com contexto de cleanup limitado, inclusive se o contexto
original foi cancelado. O serviço retorna sucesso/rejeição somente após COMMIT.

O fluxo financeiro consulta idempotência, bloqueia uma carteira com SELECT FOR
UPDATE, insere PENDING, aplica domínio, finaliza a transação e grava saldo, ledger
e eventos antes de COMMIT. Não confirma PENDING isoladamente.
O lock vem antes do INSERT para evitar upgrade dos locks KEY SHARE adquiridos
por FKs. A atualização também exige a versão anterior, impedindo escrita perdida.
Não há mutex local nem lock compartilhado entre carteiras independentes.
READ COMMITTED retorna a versão confirmada da linha depois da espera pelo lock,
conforme a [documentação PostgreSQL](https://www.postgresql.org/docs/17/transaction-iso.html).

Idempotência usa UNIQUE por provedor/chave e provedor/ID externo. INSERT ON CONFLICT
DO NOTHING participa da corrida; um SELECT separado consulta o vencedor confirmado.
Não se confia em SELECT antes de INSERT. O hash SHA-256 cobre JSON de campos fixos
em ordem lexical, com Money canônico e referência ausente como string vazia;
exclui chave e transporte. A especificação exata e o teste de bytes estão em
[POSTGRES.md](docs/POSTGRES.md). Reutilizar ID externo com outra chave é conflito,
mesmo com payload equivalente. Replay devolve saldo e versão originais persistidos.

O schema impõe unicidade da carteira, identidades externas, OPENING por carteira
e lançamento por transação/carteira, saldo não negativo, moeda BRL, versão positiva,
metadados internos/externos distintos, FKs e equação de ledger. As equações fazem
casts NUMERIC exatos para evitar overflow intermediário no CHECK.
Triggers protegem ledger contra UPDATE/DELETE/TRUNCATE, entradas e resultados
terminais contra alteração e payloads de outbox contra edição. Migrações exigem
privilégios DDL; produção deve usar identidade DML separada para a aplicação.
Constraint triggers diferidas verificam no COMMIT que cada alteração de saldo
tem próximo número de versão e ledger correspondente, que abertura positiva tem
OPENING e crédito, e que operações processadas com movimento possuem ledger.
Um índice único impede duas operações financeiras na mesma versão da carteira.
Validações usam identidades indexadas, sem varrer o histórico completo.

BET sem saldo é persistida como REJECTED com `INSUFFICIENT_BALANCE`, sem mudança de
saldo, versão ou ledger, e com WagerTransactionRejected na mesma transação.
WIN credita e gera ledger. LOSS só registra transação e evento de processamento.
Abertura positiva registra OPENING, ledger e eventos mantendo versão 1; zero
registra somente a carteira.

Os três tipos concretos de eventos têm envelope versão 1 e payload tipado.
Após serialização, o snapshot JSONB é inserido na mesma pgx.Tx das alterações.
Outbox contém tentativas, disponibilidade, publicação e lease. A Parte 4A adiciona
publisher Fx com claim curto `FOR UPDATE SKIP LOCKED`, token aleatório e expiração,
sem lock durante SQS. Eventos avançam por carteira na ordem persistida pela 0002.
Retry usa backoff exponencial limitado. A publicação é at-least-once: crash entre
envio e confirmação pode repetir o mesmo evento. O corpo é o snapshot JSONB;
group e deduplication IDs são SHA-256 da carteira e do eventId, respectivamente.
Veja [OUTBOX.md](docs/OUTBOX.md) para recuperação, shutdown e testes reais.

O schema reforça a associação de saldo, versão, operação e ledger, mas os casos de
uso continuam responsáveis por criar os eventos na mesma transação. A soma do
ledger é comparada ao saldo nos testes. Controle de acesso ao banco e reconciliação
operacional serão complementados nas etapas futuras.

## Autenticação e autorização da Parte 6A

Keycloak real importa realm jungle e service accounts pelo Compose, com healthcheck
antes do startup da API. Providers usam client_credentials e role wagering-provider.
go-oidc verifica assinatura RS256 via JWKS, issuer, audience e expiração; a aplicação
exige sub/azp/typ, respeita nbf e extrai provider_id assinado para Principal no contexto.
RemoteKeySet cacheia chaves e atualiza quando necessário para rotação. Nenhuma chave
pública, token ou secret é hardcoded no código Go.

POST /wagering/transactions é a única rota financeira acrescentada para validar
autorização; health continua público. Middleware autentica; AuthorizedFinancialService
autoriza o provider antes do caso de uso interno. Leituras do wrapper filtram provider.
Operações internas de carteira continuam sem rota pública; wallet-internal é a role
reservada para sua exposição futura. O domínio não depende de JWT/OIDC/Fx.

SQS mantém AWS/SigV4 e policies mínimas versionadas para produtor e consumer/publisher.
O consumer usa adicionalmente wagering-messaging, autenticado no IdP, com role própria
e allowlist de providers no token; cache/renovação não alteram hash financeiro ou inbox.
O README exige credenciais/políticas do broker, sem impor JWT no payload. LocalStack
Community não demonstra enforcement IAM: policies precisam ser aplicadas em AWS ou
ambiente com esse recurso. A autorização OIDC do serviço é efetivamente validada localmente.
Decisões, fronteiras de confiança e testes estão em [AUTH.md](docs/AUTH.md).

## Limitações e trabalho pendente

Health checks públicos e uma rota de envio de operações protegida por OIDC estão
registrados. Tokens reais de providers distintos, isolamento e assinatura inválida
são testados com Keycloak. A API final de consultas/carteiras/reconciliation não
faz parte desta etapa.

As entidades, schema, repositories e casos de uso desta etapa estão implementados.
WIN com referência ainda retorna ErrUnsupportedOperation.
REFUND e ROLLBACK são resolvidos por provedor/ID externo,
com ID interno persistido, lock da carteira e índice único contra dupla devolução.
Referência ausente gera PENDING_REFERENCE durável, sem movimento. Replay não
resolve a pendência; ReferenceWorker faz isso separadamente. Detalhes em
[REFERENCES.md](docs/REFERENCES.md) e [REFERENCE_RETRY.md](docs/REFERENCE_RETRY.md).
Faltam a API HTTP completa, reconciliação com cursor, métricas e comprovação de
enforcement IAM no broker local. Publisher, consumer e ReferenceWorker estão
implementados e testados. Inbox usa chave consumer/QueueArn/messageId do envelope
e hash de corpo separado do hash financeiro. FinancialService compartilha seu
fluxo interno entre Process e ProcessIncoming; savepoint permite registrar erro
terminal sem deixar escritas parciais. Inbox e financeiro compartilham COMMIT;
DeleteMessage vem depois. Recuperação e DLQ estão em [INBOX.md](docs/INBOX.md).
Inbox pode confirmar entrega com referência pendente; sua resolução imutável não
impede a transição financeira posterior realizada pelo worker.

As imagens Docker têm tags fixas; o ambiente local usa credenciais de exemplo.
O volume PostgreSQL preserva dados; a infraestrutura Keycloak e as filas são
recriadas pelo provisionamento local. Esse Compose é destinado ao desenvolvimento.
