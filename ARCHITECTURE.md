# Arquitetura — Partes 1, 2 e 3

Esta entrega estrutura o serviço e implementa as invariantes locais do núcleo
financeiro. A Parte 3 implementa persistência financeira, processamento síncrono,
locks por carteira, idempotência persistente e registro atômico da outbox.
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
| `internal/platform/messaging` | Cliente AWS SDK v2 e checks SQS |
| `internal/transport/http` | Servidor net/http e health checks |
| `internal/domain` | Money, Wallet, WagerTransaction e WalletLedgerEntry; apenas biblioteca padrão |
| `internal/application` | Abertura de carteira e processamento de BET/WIN/LOSS compartilhável por HTTP/SQS |
| `internal/workers` | Reservado a consumidor, publisher e retry de referências |
| `migrations` | Schema financeiro UP/DOWN, runner pgx e arquivos SQL embutidos |
| `cmd/migrate` | Aplicação/reversão explícita da versão 1 |

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

Não há workers nesta etapa. Cada worker futuro deverá parar de buscar trabalho,
cancelar ou concluir suas operações, aguardar suas goroutines e terminar antes do
fechamento do banco. O consumidor só poderá remover mensagens após commit durável.

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
Outbox contém campos de tentativas, disponibilidade, publicação e lease futuro,
com índice de pendências. Não há envio, publisher ou retry nesta etapa.

O schema reforça a associação de saldo, versão, operação e ledger, mas os casos de
uso continuam responsáveis por criar os eventos na mesma transação. A soma do
ledger é comparada ao saldo nos testes. Controle de acesso ao banco e reconciliação
operacional serão complementados nas etapas futuras.

## Decisões previstas para as próximas etapas

Estas escolhas orientam a estrutura; sua implementação e comprovação ainda estão pendentes.

- Referências: pendência durável, tentativas com backoff e prazo máximo.
  Máquina de estados e códigos estáveis serão definidos antes do processamento.
- Reversões: resolução por provedor/ID externo e proteção no banco contra devolução
  duplicada; a política para combinação REFUND/ROLLBACK ainda será especificada.
- Inbox e publisher: tratamento durável atômico e publicação posterior ao commit,
  identidade estável dos eventos e recuperação de trabalho assumido por outras instâncias.
- Autenticação: Keycloak externo, OIDC e `client_credentials`; validação de assinatura,
  issuer, audience e expiração antes de expor rotas financeiras. `providerId` virá da
  identidade validada; operações de carteira exigirão role interna. Sem emissão própria de tokens.

## Limitações e trabalho pendente

Somente os health checks públicos estão registrados. Não há autenticação efetiva
na API porque ainda não existem endpoints de negócio. O provisionamento Keycloak
prepara identidades locais, mas não comprova validação de tokens ou isolamento.

As entidades, schema, repositories e casos de uso desta etapa estão implementados.
REFUND, ROLLBACK, WIN com referência e espera persistente ainda não são processados;
o serviço retorna ErrUnsupportedOperation, sem efeitos financeiros, para essas entradas.
Faltam rotas financeiras, autenticação/autorização efetiva, reconciliação com cursor,
inbox, workers, retry/backoff/DLQ, publisher, métricas e controles de broker.
O consumidor e o IdP da infraestrutura da Parte 1 não foram ampliados nesta etapa.

As imagens Docker têm tags fixas; o ambiente local usa credenciais de exemplo.
O volume PostgreSQL preserva dados; a infraestrutura Keycloak e as filas são
recriadas pelo provisionamento local. Esse Compose é destinado ao desenvolvimento.
