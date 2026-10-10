.PHONY: tools fmt fmt-all fmt-check build lint arch test check
export FILES
tools:
	bash scripts/install-go-quality.sh
fmt:
	bash scripts/go-quality.sh fmt
fmt-all:
	bash scripts/go-quality.sh fmt-all
fmt-check build lint arch test check:
	bash scripts/go-quality.sh $@
