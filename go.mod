module github.com/barryq93/promDB2ORA

go 1.22.1

toolchain go1.24.0

require (
	github.com/fsnotify/fsnotify v1.7.0
	github.com/godror/godror v0.44.2
	github.com/gojek/heimdall/v7 v7.0.3
	github.com/ibmdb/go_ibm_db v0.5.2
	github.com/juju/ratelimit v1.0.2
	github.com/prometheus/client_golang v1.19.0
	github.com/sirupsen/logrus v1.9.3
	github.com/stretchr/testify v1.8.4
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/DataDog/datadog-go v3.7.1+incompatible // indirect
	github.com/afex/hystrix-go v0.0.0-20180502004556-fa1af6a1f4f5 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cactus/go-statsd-client/statsd v0.0.0-20200423205355-cb0885a1018c // indirect
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/go-logfmt/logfmt v0.6.0 // indirect
	github.com/go-logr/logr v1.4.1 // indirect
	github.com/godror/knownpb v0.1.2 // indirect
	github.com/gojek/valkyrie v0.0.0-20180215180059-6aee720afcdf // indirect
	github.com/gopherjs/gopherjs v0.0.0-20181103185306-d547d1d9531e // indirect
	github.com/ibmruntimes/go-recordio/v2 v2.0.0-20240416213906-ae0ad556db70 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/prometheus/client_model v0.5.0 // indirect
	github.com/prometheus/common v0.48.0 // indirect
	github.com/prometheus/procfs v0.12.0 // indirect
	github.com/rcrowley/go-metrics v0.0.0-20201227073835-cf1acfcdf475 // indirect
	github.com/stretchr/objx v0.5.0 // indirect
	golang.org/x/exp v0.0.0-20240719175910-8a7402abbf56 // indirect
	golang.org/x/sys v0.16.0 // indirect
	google.golang.org/protobuf v1.34.2 // indirect
)

replace github.com/gojek/heimdall/v6 => github.com/gojektech/heimdall/v6 v6.1.0

replace github.com/gojek/hystrix-client-go v0.0.0-20210215054605-8377e11e8f5d => github.com/gojek/heimdall/v7 v7.0.3
