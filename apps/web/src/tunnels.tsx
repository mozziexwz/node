import { useState } from "react";
import { Plus, ArrowUp, ArrowDown, X } from "lucide-react";
import { api, post, array, RecordData } from "./api";
import {
  Header,
  Button,
  Field,
  Select,
  Check,
  Notice,
  Table,
  Modal,
  Badge,
  Empty,
  ErrorNotice,
  AsyncForm,
  useData,
} from "./ui";

type Stage = {
  agentIds: string[];
  protocol: string;
  strategy: string;
  connectIp: string;
};
const newStage = (): Stage => ({
  agentIds: [],
  protocol: "tls",
  strategy: "round",
  connectIp: "",
});
const addresses = (node?: RecordData): string[] =>
  node
    ? [
        ...new Set<string>(
          [node.address, ...(node.addresses || [])].filter(Boolean),
        ),
      ]
    : [];
const modes: Record<string, string> = {
  both: "双向（上传 + 下载）",
  upload: "单向（仅上传）",
  download: "单向（仅下载）",
};

function StageFields({
  value,
  nodes,
  onChange,
}: {
  value: Stage;
  nodes: RecordData[];
  onChange: (stage: Stage) => void;
}) {
  const selected = nodes.filter((node) => value.agentIds.includes(node.id));
  const common = addresses(selected[0]).filter((ip) =>
    selected.every((node) => addresses(node).includes(ip)),
  );
  return (
    <>
      <div className="form-grid">
        <Select
          label="节点"
          required
          value={value.agentIds[0] || ""}
          onChange={(e) =>
            onChange({
              ...value,
              agentIds: e.target.value ? [e.target.value] : [],
              connectIp: "",
            })
          }
        >
          <option value="">选择节点</option>
          {nodes.map((node) => (
            <option key={node.id} value={node.id}>
              {node.name}
              {node.online ? "" : " · 离线"}
            </option>
          ))}
        </Select>
        <Select
          label="协议"
          value={value.protocol}
          onChange={(e) => onChange({ ...value, protocol: e.target.value })}
        >
          <option value="tcp">TCP</option>
          <option value="tls">TLS（验证节点证书）</option>
        </Select>
        <Select
          label="负载策略"
          value={value.strategy}
          onChange={(e) => onChange({ ...value, strategy: e.target.value })}
        >
          <option value="round">轮询</option>
          <option value="rand">随机</option>
          <option value="fifo">主备顺序</option>
        </Select>
      </div>
      <Select
        label="连接 IP"
        value={value.connectIp}
        onChange={(e) => onChange({ ...value, connectIp: e.target.value })}
      >
        <option value="">默认连接 IP（按地址偏好）</option>
        {common.map((ip) => (
          <option key={ip} value={ip}>
            {ip}
          </option>
        ))}
      </Select>
      <small className="muted">
        只可选择节点已登记的公网地址；候选池使用所有已选节点共有的地址。
      </small>
      <details className="mt16">
        <summary>
          候选节点池（已选 {value.agentIds.length} 个，最多 8 个）
        </summary>
        <div className="grid2 mt16">
          {nodes.map((node) => (
            <Check
              key={node.id}
              label={node.name}
              checked={value.agentIds.includes(node.id)}
              disabled={
                !value.agentIds.includes(node.id) && value.agentIds.length >= 8
              }
              onChange={(e) =>
                onChange({
                  ...value,
                  agentIds: e.target.checked
                    ? [...value.agentIds, node.id]
                    : value.agentIds.filter((id) => id !== node.id),
                  connectIp: "",
                })
              }
            />
          ))}
        </div>
      </details>
    </>
  );
}

export function RouteBuilder() {
  const { data, error, reload } = useData("/api/admin/routes");
  const { data: nodeData, error: nodeError } = useData(
    "/api/admin/relay-agents",
  );
  const nodes = array(nodeData, "agents");
  const [editing, setEditing] = useState<RecordData | null>(null);
  const [message, setMessage] = useState("");
  function open(route?: RecordData) {
    setEditing(
      route
        ? {
            ...structuredClone(route),
            exit: {
              ...newStage(),
              ...route.exit,
              agentIds: route.exit?.agentIds || [],
            },
            entryAddressesText: route.entryAuto
              ? ""
              : (
                  route.entryAddresses ||
                  (route.entryAddress ? [route.entryAddress] : [])
                ).join("\n"),
            multiplierText: String(
              (route.trafficMultiplierPermille || 1000) / 1000,
            ),
          }
        : {
            name: "",
            type: "port_forward",
            trafficMode: "both",
            multiplierText: "1",
            entryAddressesText: "",
            addressPreference: "auto",
            entryAgentId: "",
            hops: [],
            exit: newStage(),
            requireFront: false,
            enabled: false,
            rateMbps: 100000,
          },
    );
  }
  const change = (patch: RecordData) =>
    setEditing((current) => (current ? { ...current, ...patch } : current));
  function moveHop(index: number, offset: number) {
    if (!editing) return;
    const hops = [...editing.hops];
    [hops[index], hops[index + offset]] = [hops[index + offset], hops[index]];
    change({ hops });
  }
  return (
    <>
      <Header title="隧道管理" sub="先新增节点，再创建端口转发或多级隧道。">
        <Button primary onClick={() => open()} disabled={!nodes.length}>
          <Plus size={16} />
          新增隧道
        </Button>
      </Header>
      <ErrorNotice error={error || nodeError || message} />
      {!nodes.length && (
        <Notice>请先在“节点”中新增中转节点并安装 Agent。</Notice>
      )}
      <div className="card flush mt16">
        <Table
          headers={[
            "隧道",
            "类型",
            "入口节点",
            "转发链",
            "流量计算",
            "状态",
            "操作",
          ]}
          rows={array(data, "routes").map((route) => [
            route.name,
            route.type === "port_forward" ? "端口转发" : "隧道转发",
            nodes.find((node) => node.id === route.entryAgentId)?.name ||
              "节点不可用",
            route.type === "port_forward"
              ? "直接转发"
              : `${route.hops?.length || 0} 个中间跳 · ${route.exit?.agentIds?.length || 0} 个出口候选`,
            `${modes[route.trafficMode] || modes.both} × ${(route.trafficMultiplierPermille || 1000) / 1000}`,
            <Badge tone={route.enabled && route.online ? "green" : "orange"}>
              {!route.enabled
                ? "未启用"
                : route.online
                  ? "节点在线"
                  : "节点离线"}
            </Badge>,
            <div className="actions">
              <Button onClick={() => open(route)}>编辑</Button>
              <Button
                onClick={async () => {
                  if (
                    !confirm("删除此隧道？仍有关联用户规则时服务端会阻止删除。")
                  )
                    return;
                  try {
                    await api(`/api/admin/routes/${route.id}`, {
                      method: "DELETE",
                    });
                    reload();
                    setMessage("");
                  } catch (e) {
                    setMessage((e as Error).message);
                  }
                }}
              >
                删除
              </Button>
            </div>,
          ])}
        />
      </div>
      {editing && (
        <Modal
          title={editing.id ? "编辑隧道" : "新增隧道"}
          onClose={() => setEditing(null)}
        >
          <p className="muted">创建新的隧道配置</p>
          <AsyncForm
            label={editing.id ? "保存隧道" : "创建隧道"}
            onSubmit={async () => {
              if (!/^\d+(\.\d{1,3})?$/.test(editing.multiplierText))
                throw new Error("流量倍率最多三位小数");
              const multiplier = Math.round(
                Number(editing.multiplierText) * 1000,
              );
              if (multiplier < 1 || multiplier > 100000)
                throw new Error("流量倍率须为 0.001–100");
              const payload = {
                name: editing.name,
                type: editing.type,
                trafficMode: editing.trafficMode,
                trafficMultiplierPermille: multiplier,
                entryAgentId: editing.entryAgentId,
                entryAddresses: editing.entryAddressesText
                  .split(/\r?\n/)
                  .map((v: string) => v.trim())
                  .filter(Boolean),
                addressPreference: editing.addressPreference,
                hops: editing.type === "tunnel" ? editing.hops : [],
                exit:
                  editing.type === "tunnel" ? editing.exit : { agentIds: [] },
                requireFront: editing.requireFront,
                enabled: editing.enabled,
                rateMbps: Number(editing.rateMbps),
              };
              await post(
                "/api/admin/routes" + (editing.id ? `/${editing.id}` : ""),
                payload,
                editing.id ? "PUT" : "POST",
              );
              setEditing(null);
              reload();
            }}
          >
            <Field
              className="mt16"
              label="隧道名称"
              value={editing.name}
              onChange={(e) => change({ name: e.target.value })}
              required
              maxLength={100}
            />
            <Select
              label="隧道类型"
              value={editing.type}
              onChange={(e) => change({ type: e.target.value })}
            >
              <option value="port_forward">端口转发</option>
              <option value="tunnel">隧道转发（多级转跳）</option>
            </Select>
            <div className="form-grid">
              <Select
                label="流量计算"
                value={editing.trafficMode}
                onChange={(e) => change({ trafficMode: e.target.value })}
              >
                {Object.entries(modes).map(([value, text]) => (
                  <option key={value} value={value}>
                    {text}
                  </option>
                ))}
              </Select>
              <Field
                label="流量倍率"
                type="number"
                min="0.001"
                max="100"
                step="0.001"
                required
                value={editing.multiplierText}
                onChange={(e) => change({ multiplierText: e.target.value })}
              />
            </div>
            <label className="field">
              <span>入口 IP</span>
              <textarea
                rows={3}
                placeholder="留空自动使用入口节点地址；手动填写时每行一个公网 IP 或域名"
                value={editing.entryAddressesText}
                onChange={(e) => change({ entryAddressesText: e.target.value })}
              />
              <small>
                客户端配置只使用一个入口，按地址偏好选择。此处仅管理员可见。
              </small>
            </label>
            {editing.type === "tunnel" && (
              <Select
                label="隧道连接地址偏好"
                value={editing.addressPreference}
                onChange={(e) => change({ addressPreference: e.target.value })}
              >
                <option value="auto">自动选择</option>
                <option value="ipv4">优先 IPv4（需节点登记 IPv4）</option>
                <option value="ipv6">优先 IPv6（需节点登记 IPv6）</option>
              </Select>
            )}
            <section className="card mt16">
              <h3>入口配置</h3>
              <div className="mt16">
                <Select
                  label="入口节点"
                  required
                  value={editing.entryAgentId}
                  onChange={(e) => change({ entryAgentId: e.target.value })}
                >
                  <option value="">选择入口节点</option>
                  {nodes.map((node) => (
                    <option key={node.id} value={node.id}>
                      {node.name}
                      {node.online ? "" : " · 离线"}
                    </option>
                  ))}
                </Select>
              </div>
            </section>
            {editing.type === "tunnel" && (
              <>
                <section className="mt24">
                  <div className="between">
                    <h3>转发链配置</h3>
                    <Button
                      disabled={editing.hops.length >= 8}
                      onClick={() =>
                        change({ hops: [...editing.hops, newStage()] })
                      }
                    >
                      <Plus size={16} />
                      添加一跳
                    </Button>
                  </div>
                  {!editing.hops.length && (
                    <Empty>尚未添加中间跳，将由入口直接连接出口。</Empty>
                  )}
                  {editing.hops.map((hop: Stage, index: number) => (
                    <div key={index} className="card mt16">
                      <div className="between">
                        <h3>第 {index + 1} 跳</h3>
                        <div className="actions">
                          <Button
                            aria-label={`上移第${index + 1}跳`}
                            disabled={index === 0}
                            onClick={() => moveHop(index, -1)}
                          >
                            <ArrowUp size={16} />
                          </Button>
                          <Button
                            aria-label={`下移第${index + 1}跳`}
                            disabled={index === editing.hops.length - 1}
                            onClick={() => moveHop(index, 1)}
                          >
                            <ArrowDown size={16} />
                          </Button>
                          <Button
                            aria-label={`删除第${index + 1}跳`}
                            onClick={() =>
                              change({
                                hops: editing.hops.filter(
                                  (_: Stage, i: number) => i !== index,
                                ),
                              })
                            }
                          >
                            <X size={16} />
                          </Button>
                        </div>
                      </div>
                      <div className="mt16">
                        <StageFields
                          value={hop}
                          nodes={nodes}
                          onChange={(stage) =>
                            change({
                              hops: editing.hops.map((old: Stage, i: number) =>
                                i === index ? stage : old,
                              ),
                            })
                          }
                        />
                      </div>
                    </div>
                  ))}
                </section>
                <section className="card mt24">
                  <h3>出口配置</h3>
                  <div className="mt16">
                    <StageFields
                      value={editing.exit}
                      nodes={nodes}
                      onChange={(exit) => change({ exit })}
                    />
                  </div>
                </section>
              </>
            )}
            <details className="mt24">
              <summary>速率与访问设置</summary>
              <div className="mt16">
                <Field
                  label="每端口速率上限（Mbps）"
                  type="number"
                  min={1}
                  max={100000}
                  required
                  value={editing.rateMbps}
                  onChange={(e) => change({ rateMbps: e.target.value })}
                  hint="实际速率同时受用户套餐限制，上下行分别限速。"
                />
                <Check
                  checked={editing.requireFront}
                  onChange={(e) => change({ requireFront: e.target.checked })}
                  label="此隧道要求客户使用自备前置机（仅隧道级）"
                />
              </div>
            </details>
            <Check
              className="mt16"
              checked={editing.enabled}
              onChange={(e) => change({ enabled: e.target.checked })}
              label="启用隧道"
            />
            <Notice tone="orange">
              只有真实节点心跳与监听 ACK 才会激活转发。TLS
              仅用于节点之间；不改变客户 MSBOOST
              目标认证。已有用户规则时需先迁移再编辑。
            </Notice>
          </AsyncForm>
        </Modal>
      )}
    </>
  );
}
