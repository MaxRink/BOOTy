//go:build e2e_prodtpl

package integration

import (
	"cmp"
	"os"
	"slices"
)

// prodtplVariant is the template variant the lab runs (#630 section 4.3
// matrix): PRODTPL_VARIANT from test/e2e/clab/prodtpl-variant.sh, which
// also sets the PRODTPL_* files of the topology at deploy: "two-uplinks"
// (default), "one-uplink", "native-model" or "caprf-old" (one uplink).
var prodtplVariant = cmp.Or(os.Getenv("PRODTPL_VARIANT"), "two-uplinks")

// prodtplUplinks are the server NICs that the template peers on.
var prodtplUplinks = []string{"ens1f0np0", "ens1f1np1"}

func init() {
	if prodtplVariant != "one-uplink" && prodtplVariant != "caprf-old" {
		return
	}
	prodtplUplinks = prodtplUplinks[:1]
	prodtplDevices = slices.DeleteFunc(prodtplDevices, func(d string) bool { return d == "ens1f1np1" })
	for _, n := range []*prodtplNode{&prodtplBootyNode, &prodtplShadowNode, &prodtplNativeNode, &prodtplNativeVRFNode} {
		n.leafPorts = n.leafPorts[:1]
	}
	for i := range prodtplServers {
		prodtplServers[i].leafPorts = prodtplServers[i].leafPorts[:1]
	}
}
