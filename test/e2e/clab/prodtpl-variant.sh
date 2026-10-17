#!/bin/sh
# usage: eval "$(test/e2e/clab/prodtpl-variant.sh VARIANT)"
# Prints the environment for the production template lab variant:
# two-uplinks (default), one-uplink, or native-model (two uplinks; the
# native server uses NETWORK_DEVICES and BGP_SESSIONS).
native=""
case "${1:-two-uplinks}" in
two-uplinks)
	suffix="" ;;
one-uplink)
	suffix="-one-uplink" ;;
native-model)
	suffix="" native="-model" ;;
*)
	echo "unknown variant $1" >&2
	exit 1 ;;
esac
echo "export PRODTPL_VARIANT=${1:-two-uplinks}"
echo "export PRODTPL_BOOTY_FS=data/fs-prodtpl-booty${suffix}"
echo "export PRODTPL_SHADOW_FRR=prodtpl-shadow${suffix}.frr.conf"
echo "export PRODTPL_SHADOW_SETUP=prodtpl-shadow-setup${suffix}.sh"
echo "export PRODTPL_LEAF_SETUP=prodtpl-leaf-setup${suffix}.sh"
echo "export PRODTPL_NATIVE_VARS=data/vars-prodtpl-native${native:-${suffix}}"
