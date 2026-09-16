# Tailscale for Silo

Access Silo privately over HTTPS through your Tailscale network. Silo login still
applies. No separate Tailscale daemon or port forwarding is needed.

## Setup

Requires Silo’s [network-access support (PR #1096)](https://github.com/Silo-Server/silo-server/pull/1096)
and **MagicDNS** and **HTTPS certificates** enabled in your Tailscale admin console.

1. [Build the plugin](docs/DEVELOPMENT.md#build-and-verify) for your Silo host.
   Extract the ZIP, upload its `plugin` executable to Silo, and enable it.
2. Set a **Hostname** and optionally a Tailscale **Auth key**. Leave the key blank
   to sign in interactively. Use a reusable key if you have proxy nodes.
3. Save, then select **Connect** in **Settings > Network Access**. Follow the
   authorization link or approve the device if prompted.
4. Once connected, use the reported HTTPS URL from a device running Tailscale.

The API host uses your hostname exactly; proxy nodes add `-proxy-<node id>`.
Tailscale may adjust duplicate names. Certificate names appear in public
certificate transparency logs, so avoid sensitive hostnames.

HTTPS is automatic, including certificate setup after a hostname change.
Saving settings restarts a connected plugin; a disconnected plugin stays
disconnected until you select **Connect**. Certificate issuance can take time;
status reports when HTTPS is ready.

## Options

- **Tags:** Optional comma-separated tags, such as `tag:silo,tag:media`.
  Your Tailscale policy or auth key must authorize them.
- **Exit nodes:** Not supported by this plugin.

### Funnel

Off by default. Enabling Funnel makes Silo’s HTTPS endpoint publicly accessible.

> CAUTION: Exposes Silo to the open public internet, you probably don't want to do this.
> This feature requires Funnel authorization in your Tailscale account and ACL policy file.
> Funnel is not well suited to streaming video. Proceed at your own risk.

Enabled Jellyfin and Audiobookshelf endpoints remain private. Allow their ports
in your tailnet policy: **443** for Silo, **8096** for Jellyfin, and **13378** for
Audiobookshelf by default. All use HTTPS.

See [development and technical reference](docs/DEVELOPMENT.md) for builds,
tests, supported host layouts, and implementation details.
