.PHONY: integration tools fmt fmt-all fmt-check build lint arch test check quality-test
tools:
	bash scripts/install-go-quality.sh
fmt:
	bash scripts/go-quality.sh fmt $(FILES)
fmt-all:
	bash scripts/go-quality.sh fmt-all
fmt-check build lint arch test check:
	bash scripts/go-quality.sh $@
quality-test:
	bash tests/quality/check.sh
# Needs the durable test infrastructure: UMIR_TEST_DATABASE_URL and UMIR_TEST_AMQP_URL
# (umir-factory scripts/durable-test-infra.sh env). The suite never skips.
integration:
	GOWORK=off go test -tags integration -p 1 -race -count=1 ./...
