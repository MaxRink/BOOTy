module github.com/telekom/BOOTy

go 1.27.2

require (
	github.com/dustin/go-humanize v1.1.0
	github.com/fsnotify/fsnotify v1.10.1
	github.com/google/go-containerregistry v0.22.1
	github.com/google/go-tpm v0.9.8
	github.com/gopacket/gopacket v1.7.4
	github.com/insomniacslk/dhcp v0.0.0-20260901064844-234b97448fae
	github.com/klauspost/compress v1.20.3
	github.com/mdlayher/packet v1.2.0
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
	github.com/docker/cli v29.9.1+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.9.10 // indirect
	github.com/eapache/channels v1.1.0 // indirect
	github.com/eapache/queue v1.1.0 // indirect
	github.com/gaissmai/bart v0.30.0 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/josharian/native v1.2.0 // indirect
	github.com/k-sone/critbitgo v1.4.0 // indirect
	github.com/mdlayher/socket v0.8.0 // indirect
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
	github.com/u-root/uio v0.0.0-20230220225925-ffce2a382923 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260917231906-eeb232e0883d // indirect
	google.golang.org/grpc v1.84.0 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
	gotest.tools/v3 v3.5.2 // indirect
)

// GoBGP master after 4.10.0 (57fa92f1) plus the BOOTy fixes; why, and when
// to drop it:
// docs/networking.md#gobgp-version-and-fork
replace github.com/osrg/gobgp/v4 => github.com/MaxRink/gobgp/v4 v4.10.0-booty.21

// gopacket 1.7.4 and netlink 1.3.1 plus the BOOTy fixes; why, and when to
// drop them: docs/dependency-forks.md
replace github.com/gopacket/gopacket => github.com/MaxRink/gopacket v1.7.4-booty.2

replace github.com/vishvananda/netlink => github.com/MaxRink/netlink v1.3.1-booty.1

// insomniacslk/dhcp 234b97448fae plus the BOOTy fixes; why, and when to
// drop it: docs/dependency-forks.md
replace github.com/insomniacslk/dhcp => github.com/MaxRink/dhcp v0.0.0-20260901064844-234b97448fae-booty.2
