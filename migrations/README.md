# Migrations

`0001_financial.up.sql` cria wallets, wager_transactions, wallet_ledger_entries e
outbox_events, com constraints e triggers. O arquivo DOWN remove essas tabelas e
seus dados. Inbox é adicionada separadamente pela 0003.

`0002_outbox_delivery` acrescenta ordem de envio, carteira derivada do snapshot e
índice de pendências por carteira. Reutiliza os campos de lease da 0001 intacta.
UP aplica versões pendentes em ordem; DOWN reverte todas em ordem inversa,
mantendo o contrato de remover o schema financeiro. Cada comando é atômico.

`0003_inbox` cria a inbox persistente, com unicidade por consumer/source/messageId,
FK financeira, imutabilidade de resultado e validação de resolução no commit.
As versões 0001/0002 não são alteradas. DOWN também remove inbox e seus dados.

`0004_references` acrescenta rastreio para resolução futura, índice único por
referência de REFUND processado, validação SQL do contexto/valor e inbox com
resolução durável PENDING_REFERENCE. Versões 0001–0003 permanecem intactas.
Consulte [REFERENCES.md](../docs/REFERENCES.md) para regras e testes da Parte 5A.

`0005_reference_retry` cria a fila persistente de tentativas/leases, agenda pendings
legados, protege a unicidade conjunta REFUND/ROLLBACK, valida referências de ROLLBACK
e a direção do ledger, e amplia a inbox sem alterar sua resolução histórica.
Versões 0001–0004 permanecem intactas. Veja
[REFERENCE_RETRY.md](../docs/REFERENCE_RETRY.md). O DOWN restaura a constraint de
inbox como NOT VALID para preservar resoluções existentes até o runner remover
todas as versões na mesma transação. Não use DOWN como reset da fila em produção:
tentativas e leases são descartados; um novo UP reagenda pendings com attempts=0.

Defina DATABASE_URL e execute `go run ./cmd/migrate up` ou, para remover o schema
financeiro, `go run ./cmd/migrate down`. A aplicação não aplica migrations no startup.

O runner usa pgx, transação SQL, registro de versão e checksum e lock de migrations.
Veja [docs/POSTGRES.md](../docs/POSTGRES.md) para comandos completos e testes reais.
