# Contributing

Open an issue describing the problem or proposal before a substantial change.
Small fixes can be submitted directly as pull requests.

Run the seeded Docker environment with `make dev`. Before submitting, run:

```sh
go vet ./...
make integration
python3 -m unittest discover -s scripts -p 'test_*.py'
```

Use synthetic data and organization-neutral examples. Keep deployment
credentials, school assets, planning notes, and recipient lists outside Git.
The database migrations must preserve existing installations. Test delivery
against Mailpit or isolated mock relays, never real recipients.

Use Conventional Commits. Publishing happens only from the `release` branch;
container images target linux/amd64.

Contributions are provided under the repository’s MIT license.
