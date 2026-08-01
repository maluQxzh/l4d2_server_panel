import type { InstanceRecord, NodeRecord } from "./types";

export function fleetNetworkTotals(nodes: Pick<NodeRecord, "network_rx" | "network_tx">[]) {
  return nodes.reduce((total, node) => ({
    rx: total.rx + Math.max(0, Number(node.network_rx) || 0),
    tx: total.tx + Math.max(0, Number(node.network_tx) || 0),
  }), { rx: 0, tx: 0 });
}

export function availableInstanceUpdates(instances: Pick<InstanceRecord, "update_available">[]) {
  return instances.filter((instance) => instance.update_available).length;
}
