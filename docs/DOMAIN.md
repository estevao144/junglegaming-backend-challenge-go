# Domínio financeiro — Parte 2

Todo o código está em `internal/domain` e usa somente a biblioteca padrão.
Não há I/O, Fx, repositories ou processamento de mensagens nessa camada.
Os IDs e horários são fornecidos pelo chamador; o domínio não gera identidades
nem consulta o relógio. Horários válidos são convertidos para UTC.

## Money

`Money` possui dois campos privados: centavos em `int64` e moeda. Métodos
aritméticos devolvem um novo valor; nunca alteram os operandos.

```go
amount, err := domain.NewMoney("25.00", "BRL")
if err != nil {
    return err
}
zero, err := domain.ZeroMoney("BRL")
if err != nil {
    return err
}
total, err := zero.Add(amount)
if err != nil {
    return err
}
decimal, err := total.Decimal() // "25.00"
```

- Entrada externa: `NewMoney` aceita somente formato canônico não negativo,
  com duas casas e sem zeros extras à esquerda: `0.00`, `0.01`, `25.00`.
  `25`, `25.0`, `025.00`, sinais, espaços, notação científica e vírgulas são rejeitados.
  Não há arredondamento nem normalização. O hash futuro deve usar a representação
  retornada por `Decimal`, mantendo a mesma política em HTTP e SQS.
- Moeda operacional: somente BRL, código ISO 4217, com escala fixa de duas casas.
  Todos os construtores rejeitam outras moedas, inclusive USD. O tipo mantém
  `currency`, e soma, subtração e comparação rejeitam moedas incompatíveis.
  Os testes simulam valores de moeda diferente usando os campos privados do
  pacote, sem habilitar suporte operacional a outra moeda. Ampliar o suporte
  exigirá uma decisão explícita de domínio e escala.
- `MoneyFromMinorUnits` aceita valores assinados, para reidratação e cálculos
  internos. Adaptadores de entrada devem usar `NewMoney`, não esse construtor.
- Intervalo interno: de `-92233720368547758.08` a `92233720368547758.07`.
  Parsing, soma, subtração e negação detectam overflow. Negar o mínimo de `int64`
  retorna erro; sua serialização decimal é válida.
- `Compare` retorna -1, 0 ou 1; soma, subtração e comparação exigem mesma moeda.
- `MarshalJSON` produz `{"amount":"25.00","currency":"BRL"}`. Não há unmarshaler:
  os adaptadores futuros devem ler strings e chamar o construtor validado.
- O zero value `Money{}` é inválido, pois não possui moeda. Use `ZeroMoney`.

## Wallet

`NewWallet(id, playerID, initialBalance, at)` cria versão 1. Saldo inicial zero
é aceito. `RehydrateWallet(WalletState)` mantém o saldo e a versão fornecidos,
sem executar crédito, débito ou emissão de eventos.

`Credit(money, at)` e `Debit(money, at)` alteram somente o estado privado. Ambos
validam carteira, moeda, valor, timestamp e overflow antes de qualquer mutação.
Débito exige saldo suficiente. Crédito e débito exigem valor estritamente positivo:
zero e valores negativos retornam `ErrInvalidMoney`, sem alterar saldo, versão ou
`updatedAt`. Saldo inicial zero continua permitido na criação da carteira.
LOSS não chama `Credit` ou `Debit`: registra somente o resultado da transação,
preservando o saldo, a versão e os timestamps da carteira.

`WalletState` e `Snapshot()` são cópias para leitura e futura persistência.
Alterar o snapshot não altera a entidade. Versões são `int64`, positivas e com
overflow protegido, para mapeamento direto ao `BIGINT` do PostgreSQL.
O timestamp de uma mudança não pode preceder o último estado; igualdade é aceita.

Uma Wallet é mutável e deve pertencer ao processamento de uma única operação.
Não é um objeto compartilhado protegido por mutex. O controle entre processos
continuará sendo responsabilidade do banco e da transação SQL.

## WagerTransaction

`TransactionData` reúne a entrada imutável; `WagerTransactionState` acrescenta
status, referência interna, código de falha, resultado e timestamps.
`NewWagerTransaction(data, at)` aceita somente os cinco tipos externos, valida
metadados e cria `PENDING`. A chave recebida é preservada, sem substituição.
O hash é representado como string não vazia: seu cálculo e confronto ficam para
a camada de aplicação, sem implementação de idempotência nesta etapa.

| Tipo | Valor | Movimento / referência |
| --- | --- | --- |
| BET | Positivo | Débito; sem referência |
| WIN | Positivo | Crédito; referência externa opcional |
| LOSS | Zero | `NoMovement`; sem referência |
| REFUND | Positivo | Crédito; referência externa obrigatória |
| ROLLBACK | Positivo | Referência externa obrigatória; movimento depende do original |
| OPENING | Positivo | Crédito interno; sem metadados externos |

BET e LOSS não aceitam referências nesta interpretação, pois o enunciado só as
define para WIN e reversões. Autorrefências externas ou internas são rejeitadas.
`Movement()` retorna `ErrUnresolvedReference` para ROLLBACK: a resolução futura
determinará a direção inversa, sem assumir que toda reversão é crédito.

`NewOpeningTransaction` aceita identidade interna estável, carteira, jogador,
Money positivo e horário, sem exigir nem aceitar metadados externos.
Cria `PENDING`, conforme a regra geral de criação. O futuro caso de uso de abertura
deve chamar `MarkProcessed` e confirmar OPENING, ledger e carteira no mesmo commit,
sem publicar ou confirmar uma abertura parcialmente processada.
O resultado de OPENING processado deve ter o saldo inicial e versão 1.
Saldo inicial zero não deve chamar esse construtor nem criar lançamento financeiro.

| Origem | Destinos válidos |
| --- | --- |
| PENDING | PENDING_REFERENCE, PROCESSED, REJECTED, FAILED |
| PENDING_REFERENCE | PROCESSED, REJECTED, FAILED |
| PROCESSED / REJECTED / FAILED | Nenhum |

Métodos: `MarkPendingReference`, `MarkProcessed`, `Reject`, `Fail`.
Toda mudança passa por `TransactionStatus.canTransitionTo`, que declara
explicitamente os destinos permitidos por estado. Autotransições, estados de
destino desconhecidos e qualquer transição a partir de um terminal são rejeitados
com `ErrInvalidTransition`. Os testes cobrem cada par de origem/destino, incluindo
retorno a PENDING e destinos desconhecidos, sem alterar o estado nas rejeições.
Entrar em espera exige referência externa. Não há transição de espera de volta
para PENDING; retries mantêm a espera até resultado definitivo.
Processamento com referência externa exige registrar também a identidade interna
resolvida, inclusive em WIN com referência opcional informada.
O relacionamento financeiro com o original ainda não é resolvido nem validado aqui.

`FinancialResult` contém saldo original e versão da carteira. É obrigatório em
PROCESSED e opcional em REJECTED; rejeições sem saldo disponível para resposta
podem usar nil. Esse resultado deve ser persistido para replay, sem consultar
o saldo atual. Na reidratação e em `Snapshot()`, o resultado opcional é copiado
para impedir alteração da entidade por um ponteiro do chamador.

| Código estável | Finalidade prevista |
| --- | --- |
| INSUFFICIENT_BALANCE | Aposta sem saldo |
| REVERSAL_INSUFFICIENT_BALANCE | Reversão cujo débito não cabe no saldo |
| REFERENCE_NOT_FOUND | Referência não encontrada após limite de espera |
| REFERENCE_NOT_PROCESSED | Referência terminou sem sucesso |
| INVALID_REFERENCE | Referência incompatível |
| REVERSAL_CONFLICT | Reversão financeira duplicada ou incompatível |
| INFRASTRUCTURE_PERMANENT | Falha permanente auditável |

`Reject` exige um código de negócio conhecido; `Fail` define automaticamente o
código de infraestrutura permanente. Erros transitórios não chamam `Fail` nem
mudam a entidade: a futura aplicação decide retry com contexto de infraestrutura.
A classificação real, o TTL de referências e a aplicabilidade de cada rejeição
a uma operação serão responsabilidade dos futuros casos de uso.

`RehydrateWagerTransaction` valida coerência do estado completo e restaura o
snapshot sem aplicar dinheiro, transições ou eventos. Estados terminais não podem
ser alterados nem em um replay. Replay é leitura, não nova chamada a `MarkProcessed`.

## WalletLedgerEntry

O ledger tem campos privados, um snapshot por valor e nenhum método de alteração.
`NewWalletLedgerEntry` e `RehydrateWalletLedgerEntry` validam IDs, horário, moeda,
valor positivo, saldos não negativos, direção e equação exata:

```text
CREDIT: balanceAfter = balanceBefore + money
DEBIT:  balanceAfter = balanceBefore - money
```

Zero e `NoMovement` são rejeitados. LOSS e transações rejeitadas não devem levar
à criação de ledger. Como o construtor recebe apenas transactionId, essa relação
será imposta pela orquestração futura; não há lookup ou processamento embutido.
Reidratar um lançamento apenas valida a equação, sem alterar qualquer carteira.

## Erros e pontos para PostgreSQL

Erros sentinela são identificáveis por `errors.Is`; contexto adicional é anexado
com `%w`. Não se usa panic para rejeições de negócio.
IDs são strings opacas não vazias, sem espaços nas extremidades. Geração e formatos
de identidade dos contratos serão tratados pelos adaptadores, sem dependências
de UUID no domínio.

- Persistir Money como centavos em `BIGINT` + moeda, saldo/version/resultados
  exatamente como validados. Não usar conversões por ponto flutuante.
- Impor unicidade `(playerId, currency)`, `(providerId, idempotencyKey)`,
  `(providerId, externalTransactionId)` e `(walletId, transactionId)` no banco.
- Confirmar saldo, versão, WagerTransaction e ledger atomicamente; eventos e inbox
  também compartilharão o commit quando forem implementados.
- Saldo não negativo e coerência monetária exigem constraints, além do domínio.
  Ledger imutável em memória não substitui proteção SQL contra UPDATE/DELETE.
- Distinguir OPENING interno no schema e impedir crédito de abertura duplicado.
- Persistir o resultado original para replay e pendências de referência de forma
  durável. Copiar snapshots não representa uma implementação de persistência.
- A Parte 3 adiciona três modelos de eventos, hash canônico, repositories e
  processamento síncrono de abertura/BET/WIN sem referência/LOSS. Veja
  [POSTGRES.md](POSTGRES.md) para a persistência dessas invariantes.
- Políticas de reversão combinada, unicidade de reversões, resolução/recuperação
  de pendências, inbox e publicação de eventos ainda serão implementadas.
