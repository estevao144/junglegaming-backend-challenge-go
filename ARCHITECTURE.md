# Arquitetura — Parte 1

Esta entrega estrutura o serviço. Nenhuma garantia financeira do desafio é
considerada implementada nesta etapa.

## Organização

| Pacote | Responsabilidade |
| --- | --- |
| `cmd/api` | Entrypoint, execução da aplicação Fx |
| `internal/app` | Composição dos módulos |
| `internal/config` | Leitura e validação de ambiente |
| `internal/platform/logging` | Logs JSON com `log/slog` |
| `internal/platform/postgres` | Pool pgx e lifecycle |
| `internal/platform/messaging` | Cliente AWS SDK v2 e checks SQS |
| `internal/transport/http` | Servidor net/http e health checks |
| `internal/domain` | Reservado a entidades e valores; sem dependências de infraestrutura |
| `internal/application` | Reservado a casos de uso e ports compartilhados por HTTP/SQS |
| `internal/workers` | Reservado a consumidor, publisher e retry de referências |
| `migrations` | Reservado a schema e migrations versionadas |

Fx faz a composição por construtores, `fx.Module`, `fx.Provide` e `fx.Invoke`.
Nenhum service locator ou dependência de Fx será introduzido no domínio.
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

## Decisões previstas para as próximas etapas

Estas escolhas orientam a estrutura; sua implementação e comprovação ainda estão pendentes.

- Persistência: pgx com SQL explícito. Casos de uso delimitarão uma única transação
  compartilhada por saldo, operação, ledger, inbox e outbox; repositórios não farão commits isolados.
- Money: `int64` em unidades mínimas + moeda, persistido em `BIGINT`, com escala
  externa fixa de duas casas. Limite previsto: 92.233.720.368.547.758,07 em módulo
  positivo. Parsing e aritmética exigirão proteção de overflow; não usar floats.
- Concorrência: lock PostgreSQL por carteira (`SELECT ... FOR UPDATE`), sem locks
  globais, com constraints para saldo não negativo, unicidade e integridade do ledger.
- Idempotência: registros e resultados persistentes, unicidade por provedor/chave
  e por provedor/ID externo, hash canônico compartilhado entre HTTP e SQS.
  Campos, normalização e códigos de resposta serão definidos com o contrato.
- Referências: pendência durável, tentativas com backoff e prazo máximo.
  Máquina de estados e códigos estáveis serão definidos antes do processamento.
- Reversões: resolução por provedor/ID externo e proteção no banco contra devolução
  duplicada; a política para combinação REFUND/ROLLBACK ainda será especificada.
- Inbox/outbox: tratamento durável atômico e publicação posterior ao commit,
  identidade estável dos eventos e recuperação de trabalho assumido por outras instâncias.
- Autenticação: Keycloak externo, OIDC e `client_credentials`; validação de assinatura,
  issuer, audience e expiração antes de expor rotas financeiras. `providerId` virá da
  identidade validada; operações de carteira exigirão role interna. Sem emissão própria de tokens.

## Limitações e trabalho pendente

Somente os health checks públicos estão registrados. Não há autenticação efetiva
na API porque ainda não existem endpoints de negócio. O provisionamento Keycloak
prepara identidades locais, mas não comprova validação de tokens ou isolamento.

Ainda faltam entidades financeiras, migrations e constraints, repositórios,
casos de uso, rotas financeiras, autenticação/autorização, workers, métricas,
correlation IDs, controles de broker e testes distribuídos e de recuperação.

As imagens Docker têm tags fixas; o ambiente local usa credenciais de exemplo.
O volume PostgreSQL preserva dados; a infraestrutura Keycloak e as filas são
recriadas pelo provisionamento local. Esse Compose é destinado ao desenvolvimento.
