update:
	bazel run //:gazelle

update-deps:
	bazel run //:gazelle -- update-repos -from_file=go.mod -to_macro=deps.bzl%go_library_dependencies

push:
	bazel build --platforms=@io_bazel_rules_go//go/toolchain:linux_amd64 //:image.tar
	docker load -i bazel-bin/image.tar
	docker tag bazel:image quay.io/f110/update-repository:latest
	docker push quay.io/f110/update-repository:latest
	docker rmi bazel:image

push-build-sidecar:
	bazel build --platforms=@io_bazel_rules_go//go/toolchain:linux_amd64 //:build-sidecar-image.tar
	docker load -i bazel-bin/build-sidecar-image.tar
	docker tag bazel:build-sidecar-image quay.io/f110/k8s-cluster-maintenance-bot-build-sidecar:latest
	docker push quay.io/f110/k8s-cluster-maintenance-bot-build-sidecar:latest
	docker rmi bazel:build-sidecar-image

.PHONY: push push-init-cmd update update-deps