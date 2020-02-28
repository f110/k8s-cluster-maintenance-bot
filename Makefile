update:
	bazel run //:gazelle

run:
	bazel run //cmd/maintenance-bot -- -c $(CURDIR)/config_debug.yaml

.PHONY: update run