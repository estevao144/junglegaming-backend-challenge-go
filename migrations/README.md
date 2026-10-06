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

Defina DATABASE_URL e execute `go run ./cmd/migrate up` ou, para remover o schema
financeiro, `go run ./cmd/migrate down`. A aplicação não aplica migrations no startup.

O runner usa pgx, transação SQL, registro de versão e checksum e lock de migrations.
Veja [docs/POSTGRES.md](../docs/POSTGRES.md) para comandos completos e testes reais.
