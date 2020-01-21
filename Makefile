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

.PHONY: push update update-deps