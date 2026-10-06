# REFUND e referências financeiras — Parte 5A

Este documento registra a entrega da Parte 5A. A Parte 5B amplia a proteção para
REFUND/ROLLBACK e implementa o worker: [REFERENCE_RETRY.md](REFERENCE_RETRY.md).
Replays continuam sem resolver pendências; o worker usa uma transação própria.

`FinancialService.Process` e `ProcessIncoming` usam o mesmo `processOperation`.
REFUND credita integralmente uma BET PROCESSED, com ledger CREDIT, próxima versão
da carteira, resultado original persistido e dois snapshots na outbox. Não há
publicação SQS dentro da transação financeira.

## Resolução e locks

A busca é exclusivamente `(providerId, referenceExternalTransactionId)`.
Uma operação de outro provedor não é uma referência encontrada: ausência no par
solicitado fica PENDING_REFERENCE. Isso evita busca global ou exposição entre
provedores. Se ambos têm o mesmo external ID, cada REFUND resolve a BET de seu
próprio provedor.

A referência deve ser BET PROCESSED e concordar em provedor, jogador, carteira,
moeda, rodada e valor exato. GameId continua registrado; o README não exige que
seja igual entre operação e referência. BRL permanece a única moeda operacional;
moeda não suportada ou valor não positivo é entrada inválida, antes de criar
REFUND. Referências existentes incompatíveis terminam REJECTED e geram evento.

Ordem: consulta inicial de idempotência; lock `FOR UPDATE` da carteira da operação;
inserção da transação própria; busca da referência em um novo snapshot SQL;
validação e verificação de REFUND já processado. A busca após o lock observa uma
BET que tenha acabado de confirmar na mesma carteira. Não há lock global nem lock
da carteira referenciada quando ela é incompatível. BETs terminais são imutáveis;
não é necessário obter FOR UPDATE da referência e criar uma ordem adicional de
locks. Carteiras independentes continuam avançando em paralelo.

O ID interno encontrado fica em `reference_transaction_id`, inclusive em
rejeições auditadas. O ID externo original permanece imutável. Replays verificam
chave, identidade externa e hash, devolvendo saldo/versão originais ou a pendência
persistida, sem novos eventos.

## Proteções e códigos

Migration `0004_references` acrescenta:

- índice único parcial `one_processed_refund_per_reference`, impedindo dois
  REFUNDs PROCESSED sobre o mesmo ID interno, mesmo por escritores SQL;
- trigger que exige BET PROCESSED com contexto e valor integral compatíveis;
- índice por created_at/id para futuras consultas de PENDING_REFERENCE;
- correlação/causação imutáveis na transação, para eventos da futura resolução;
- suporte da inbox à resolução durável PENDING_REFERENCE e aos códigos abaixo.

Os códigos existentes são reutilizados, sem novos códigos:
`INVALID_REFERENCE` (tipo/contexto/valor), `REFERENCE_NOT_PROCESSED` (BET existente
sem sucesso) e `REVERSAL_CONFLICT` (já devolvida). Essas rejeições preservam saldo
e versão observados e criam WagerTransactionRejected, sem ledger ou movimento.
Como REFUNDs válidos sobre uma BET usam sua mesma carteira, o lock serializa a
verificação e permite rejeitar o perdedor normalmente; o índice único é a segunda
proteção no banco, independente de código Go.

## Referência ausente e SQS

Ausência cria REFUND PENDING_REFERENCE e WagerTransactionPendingReference no mesmo
commit. Nenhum saldo, versão ou ledger muda. A transação conserva IDs, provedor,
chave, hash, moeda/valor, jogador/carteira/rodada/jogo, referência externa,
timestamps e correlação/causação. Referência interna e resultado permanecem vazios.
O evento tem envelope versão 1 e data tipado com a identidade e referência
externa, Money decimal e walletId para o roteamento do publisher existente.

SQS aceita `data.kind="REFUND"` com `data.referenceExternalTransactionId` obrigatório,
mantendo o contrato de [INBOX.md](INBOX.md). O hash financeiro já inclui a referência
e exclui metadados de transporte. Inbox, REFUND e outbox compartilham COMMIT.
PENDING_REFERENCE resolve a entrega atual: inbox registra esse status com
processed_at e transaction_id; depois DeleteMessage é permitido. Reentrega real
verifica a mesma inbox sem duplicar transação, crédito ou eventos. Mensagens
distintas usam também a idempotência financeira.

A inbox conserva o snapshot da entrega; isso não bloqueia a futura transição
financeira PENDING_REFERENCE → PROCESSED/REJECTED. O domínio e o schema já permitem
essas transições. A Parte 5A não tenta resolvê-las ao receber a BET ou num replay:
continuam pendentes até a Parte 5B. Não há worker, retry/TTL/attempts de referência,
resolução automática após restart ou ROLLBACK nesta etapa.

## Execução e verificação

Suba PostgreSQL/LocalStack e aplique `go run ./cmd/migrate up` antes de iniciar a API,
com DATABASE_URL configurado, conforme [INBOX.md](INBOX.md). Migrations 0001–0003
permanecem intactas. DOWN do runner reverte todas as versões e remove os dados;
UP/DOWN/UP é testado somente em schemas temporários, incluindo inbox pendente.
O DOWN da 0004 restaura as constraints antigas como NOT VALID para permitir os
novos estados até a remoção da inbox pela 0003, dentro da mesma transação do runner.

```powershell
go test ./...
go test -race ./...
go vet ./...
go test -race -tags=integration -count=1 ./...
```

Prepare as variáveis de PostgreSQL/LocalStack descritas em INBOX.md. Race requer
CGO e compilador C; o comando com container Go Linux está em OUTBOX.md.

Integrações reais cobrem REFUND integral, referências inválidas e isolamento por
provedor, pendência durável em outra instância, replay original após saldo mudar,
dois REFUNDs concorrentes e 50 cópias usando três pools, índice SQL contra duplicata,
rollback por falha na outbox, SQS/inbox com expiry real de visibility, publicação
dos eventos, replay entre transportes, transição futura via repositories somente
em teste e migrations. As suítes das Partes 3, 4A e 4B continuam incluídas.
