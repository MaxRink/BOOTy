module github.com/telekom/BOOTy

go 1.27.2

require (
	github.com/digineo/go-dhclient v1.0.2
	github.com/dustin/go-humanize v1.1.0
	github.com/fsnotify/fsnotify v1.10.1
	github.com/google/go-containerregistry v0.22.1
	github.com/google/go-tpm v0.9.8
	github.com/google/gopacket v1.1.19
	github.com/klauspost/compress v1.20.3
	github.com/osrg/gobgp/v4 v4.10.0
	github.com/pierrec/lz4/v4 v4.1.33
	github.com/spf13/cobra v1.10.2
	github.com/telekom/t-caas-go-library v0.1.1
	github.com/ulikunitz/xz v0.5.17
	github.com/vishvananda/netlink v1.3.1
	github.com/vishvananda/netns v0.0.5
	github.com/zcalusic/sysinfo v1.1.3
	golang.org/x/net v0.61.0
	golang.org/x/sys v0.49.0
	google.golang.org/protobuf v1.36.12
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/dgryski/go-farm v0.0.0-20240924180020-3414d57e47da // indirect
	github.com/docker/cli v29.9.0+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.9.10 // indirect
	github.com/eapache/channels v1.1.0 // indirect
	github.com/eapache/queue v1.1.0 // indirect
	github.com/gaissmai/bart v0.30.0 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/k-sone/critbitgo v1.4.0 // indirect
	github.com/mdlayher/packet v1.2.0 // indirect
	github.com/mdlayher/raw v0.1.0 // indirect
	github.com/mdlayher/socket v0.7.0 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/orcaman/concurrent-map/v2 v2.0.1 // indirect
	github.com/pelletier/go-toml/v2 v2.4.3 // indirect
	github.com/sagikazarmark/locafero v0.12.0 // indirect
	github.com/segmentio/fasthash v1.0.3 // indirect
	github.com/sirupsen/logrus v1.10.2 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/spf13/viper v1.21.0 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260917231906-eeb232e0883d // indirect
	google.golang.org/grpc v1.84.0 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
	gotest.tools/v3 v3.5.2 // indirect
)

// GoBGP 4.10.0 plus the BOOTy fixes; why, and when to drop it:
// docs/networking.md#gobgp-version-and-fork
replace github.com/osrg/gobgp/v4 => github.com/MaxRink/gobgp/v4 v4.10.0-booty.2
