# Tailscale for Silo

Access Silo over HTTPS through your Tailscale network. No separate Tailscale daemon or port forwarding required.

## Setup

Requires Silo’s [network-access support](https://github.com/Silo-Server/silo-server/pull/1096), with MagicDNS and HTTPS certificates enabled in Tailscale.

1. [Build the plugin](docs/DEVELOPMENT.md#build-and-verify), extract the ZIP, and upload its `plugin` executable to Silo.
2. Enable the plugin, choose a hostname, and optionally enter a Tailscale auth key.
3. Save, then select **Connect** in **Settings > Network Access**. Sign in if prompted.
4. Open the reported HTTPS URL from a device running Tailscale.

## Funnel

Optional public access. Disabled by default.

> CAUTION: Exposes Silo to the open public internet, you probably don't want to do this.
> This feature requires Funnel authorization in your Tailscale account and ACL policy file.
> Funnel is not well suited to streaming video. Proceed at your own risk.

[Developer documentation](docs/DEVELOPMENT.md)
