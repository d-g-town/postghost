# postghost 👻

A small Go message board backed by Postgres. Server-rendered HTML, no JavaScript,
no frontend build step — templates and CSS are embedded in the binary.

## Run it

```sh
docker compose up --build
```

Then open <http://localhost:8080>.

## Run the app against your own Postgres

```sh
export DB_URL='postgres://user:pass@localhost:5432/postghost?sslmode=require'
go run .
```

The `posts` table is created on startup if it doesn't exist.

## Configuration

`DB_URL` takes precedence when set. Otherwise the connection string is assembled
from the discrete variables.

| Variable     | Default      | Purpose                                     |
| ------------ | ------------ | ------------------------------------------- |
| `DB_URL`     | _(unset)_    | Full Postgres URL; overrides everything below |
| `DB_HOST`    | `localhost`  | Database host                               |
| `DB_PORT`    | `5432`       | Database port                               |
| `DB_NAME`    | `postghost`  | Database name                               |
| `DB_USER`    | `postghost`  | Database user                               |
| `DB_PASS`    | `postghost`  | Database password                           |
| `DB_SSLMODE` | `prefer`     | TLS mode; `disable` for local Postgres      |
| `PORT`       | `8080`       | HTTP listen port                            |

## Routes

| Method | Path                 | Description              |
| ------ | -------------------- | ------------------------ |
| `GET`  | `/`                  | List the 100 most recent posts |
| `POST` | `/posts`             | Create a post            |
| `POST` | `/posts/{id}/delete` | Delete a post            |
| `GET`  | `/healthz`           | Liveness + database ping |

## Notes

- The container is `distroless/static` running as `nonroot`, built `CGO_ENABLED=0`.
- On startup the app retries the database for up to 30s, so it tolerates
  Postgres still coming up alongside it.
- Post bodies are escaped by `html/template` and queries are parameterised.
