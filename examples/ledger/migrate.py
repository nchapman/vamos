"""Explicit, transactional schema initialization. Never retries schema execution."""
import sys
import time
from ledger import connect

SCHEMA = '''CREATE TABLE IF NOT EXISTS entries (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    amount_cents BIGINT NOT NULL,
    description TEXT NOT NULL CHECK (length(description) BETWEEN 1 AND 500)
)'''


def migrate(connector=connect, sleep=time.sleep, attempts=20):
    if attempts < 1:
        raise ValueError('attempts must be positive')
    for attempt in range(attempts):
        try:
            conn = connector()
            break
        except (KeyError, ImportError):
            raise  # Configuration/install problems are not startup races.
        except Exception:
            if attempt == attempts - 1:
                raise
            sleep(1)
    with conn:
        conn.execute(SCHEMA)
        # Also fail on incompatible pre-existing tables; IF NOT EXISTS is not an upgrade.
        conn.execute('SELECT id, amount_cents, description FROM entries LIMIT 0')


def main():
    try:
        migrate()
    except Exception:
        print('Schema initialization failed; check database availability/configuration and schema.', file=sys.stderr)
        return 1
    print('Ledger schema initialized.')
    return 0


if __name__ == '__main__':
    sys.exit(main())
