# Domain template tokens

`scripts/new-domain.sh` copies this tree into `internal/<name>/` and replaces the
following tokens in BOTH file contents and file names. After generation, no `__`
token should remain (the script verifies this).

| Token          | Meaning                              | Example (`invoice`)                       |
|----------------|--------------------------------------|-------------------------------------------|
| `__domain__`   | lower singular (package / dir name)  | `invoice`                                 |
| `__Domain__`   | PascalCase singular (type names)     | `Invoice`                                 |
| `__domains__`  | lower plural (table / route segment) | `invoices`                                |
| `__DOMAIN__`   | screaming upper singular             | `INVOICE`                                 |
| `__MODULE__`   | Go module path                       | `github.com/williamlabdev/saas-forge`   |
| `__MIGNUM__`   | zero-padded migration number (6)     | `000009`                                  |

Notes:
- `__DOMAIN__` is reserved for screaming-case use (e.g. env vars / constants); it
  is not currently emitted in the template body but is replaced if present so the
  scheme stays consistent.
- The plural defaults to `<name>s`; pass an explicit plural as the 2nd script arg
  for irregular nouns (e.g. `./scripts/new-domain.sh person people`).
