update:
	bazel run //:gazelle

run:
	bazel run //cmd/maintenance-bot -- -c $(CURDIR)/config_debug.yaml

push:
	bazel query 'kind(container_push, //...)' | xargs -n1 bazel run

.PHONY: update run push