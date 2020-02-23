module github.com/f110/k8s-cluster-maintenance-bot

go 1.13

require (
	github.com/aws/aws-sdk-go v1.28.8
	github.com/bradleyfalzon/ghinstallation v1.1.1
	github.com/google/go-github/v29 v29.0.2
	github.com/sergi/go-diff v1.1.0 // indirect
	github.com/spf13/pflag v1.0.5
	golang.org/x/crypto v0.0.0-20200117160349-530e935923ad // indirect
	golang.org/x/net v0.0.0-20200114155413-6afb5195e5aa // indirect
	golang.org/x/sys v0.0.0-20200121082415-34d275377bf9 // indirect
	golang.org/x/xerrors v0.0.0-20191204190536-9bdfabe68543
	gopkg.in/src-d/go-git.v4 v4.13.1
	gopkg.in/yaml.v2 v2.2.7 // indirect
	k8s.io/api v0.17.0
	k8s.io/apimachinery v0.17.0
	k8s.io/client-go v0.17.0
	sigs.k8s.io/yaml v1.1.0
)
