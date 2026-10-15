/*
 * kinora_tunnel.h: C interface to the Kinara tunnel client (kinora-tunnel-ffi).
 *
 * All strings are UTF-8 JSON. Every string returned must be released with
 * kinora_tunnel_string_free. Answers carry "ok": true plus the result, or
 * "ok": false and "error".
 *
 * Start settings (camelCase):
 *   server          connection ticket or node ID (required)
 *   dataDir         folder the app owns; the device key goes in dataDir/iroh (required)
 *   relayUrls       self-hosted iroh relays; when set, only these are used
 *   relayMode       "default" | "disabled" | "custom" (default "default")
 *   relayOnly, discoveryN0, mdns, localPort, connectTimeoutMs
 */
#ifndef KINORA_TUNNEL_H
#define KINORA_TUNNEL_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/* {"ok":true,"handle":1,"port":53123,"url":"http://127.0.0.1:53123","nodeId":"..."} */
char *kinora_tunnel_start(const char *config_json);

/* {"ok":true,"connected":true,"path":"direct-v6","rttMs":23.4,"bytesIn":...,"lastError":null,...} */
char *kinora_tunnel_status(uint64_t handle);

void kinora_tunnel_network_change(void);
void kinora_tunnel_stop(uint64_t handle);
void kinora_tunnel_stop_all(void);

/* Log lines since the last call, newline separated. */
char *kinora_tunnel_drain_logs(void);

void kinora_tunnel_string_free(char *s);

#ifdef __cplusplus
}
#endif

#endif /* KINORA_TUNNEL_H */
