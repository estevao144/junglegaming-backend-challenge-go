# Migrations

`0001_financial.up.sql` cria wallets, wager_transactions, wallet_ledger_entries e
outbox_events, com constraints e triggers. O arquivo DOWN remove essas tabelas e
seus dados. Não há inbox nesta etapa.

Defina DATABASE_URL e execute `go run ./cmd/migrate up` ou, para remover o schema
financeiro, `go run ./cmd/migrate down`. A aplicação não aplica migrations no startup.

O runner usa pgx, transação SQL, registro de versão e checksum e lock de migrations.
Veja [docs/POSTGRES.md](../docs/POSTGRES.md) para comandos completos e testes reais.
