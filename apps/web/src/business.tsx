import { useEffect, useState } from "react";
import { api, post, array, requestID, download, RecordData } from "./api";
import {
  Header,
  Button,
  Field,
  Check,
  Notice,
  Table,
  Modal,
  Select,
  Badge,
  Empty,
  ErrorNotice,
  AsyncForm,
  useData,
  money,
  date,
  gb,
} from "./ui";
import { ConfigUpload, SSHFields, SSH, prepareSSH } from "./tools";
import { PaymentReturn } from "./payment-return";
import { relayNeedsRecovery, relayStatus } from "./relay-status";
import { RelayStatusDetail } from "./relay-status-view";

export function Account({
  user,
  onRefresh,
}: {
  user: RecordData;
  onRefresh: () => void;
}) {
  const [message, setMessage] = useState("");
  return (
    <>
      <Header
        title="账户与邮箱验证"
        sub="验证当前注册邮箱，保障账号和免费工具的使用。"
      />
      <div className="card">
        <div className="between">
          <div>
            <h2>{user.email}</h2>
            <p className="muted mt8">
              {user.emailVerifiedAt
                ? "验证时间：" + date(user.emailVerifiedAt)
                : "当前邮箱尚未验证"}
            </p>
          </div>
          <Badge tone={user.emailVerifiedAt ? "green" : "orange"}>
            {user.emailVerifiedAt ? "已验证" : "未验证"}
          </Badge>
        </div>
        {!user.emailVerifiedAt && (
          <div className="mt24">
            <Button
              onClick={async () => {
                try {
                  await post("/api/auth/email/send", {
                    email: user.email,
                    purpose: "verify",
                  });
                  setMessage("验证码已发送，请检查收件箱。");
                } catch (e) {
                  setMessage((e as Error).message);
                }
              }}
            >
              获取验证码
            </Button>
            {message && (
              <div className="mt16">
                <Notice>{message}</Notice>
              </div>
            )}
            <AsyncForm
              label="确认验证"
              onSubmit={(f) =>
                post("/api/auth/email/verify", { code: f.get("code") })
              }
              onDone={onRefresh}
            >
              <div className="mt16">
                <Field
                  label="邮箱验证码"
                  name="code"
                  inputMode="numeric"
                  autoComplete="one-time-code"
                  required
                />
              </div>
            </AsyncForm>
          </div>
        )}
      </div>
    </>
  );
}
export function Wallet({ onRefresh }: { onRefresh: () => void }) {
  const { data, error, reload } = useData("/api/wallet");
  return (
    <>
      <Header title="余额与卡密" sub="兑换卡密后，余额可用于购买套餐。" />
      <ErrorNotice error={error} />
      <div className="grid2">
        <div className="card">
          <div className="eyebrow">AVAILABLE BALANCE</div>
          <div className="stat-value">¥ {money(data?.balanceCents)}</div>
        </div>
        <div className="card">
          <h3>兑换卡密</h3>
          <AsyncForm
            label="确认兑换"
            onSubmit={(f) =>
              post("/api/wallet/redeem", {
                code: f.get("code"),
                requestId: requestID(),
              })
            }
            onDone={() => {
              reload();
              onRefresh();
            }}
          >
            <div className="mt16">
              <Field
                label="卡密"
                name="code"
                placeholder="请输入完整卡密"
                required
              />
            </div>
          </AsyncForm>
        </div>
      </div>
      <div className="card flush mt24">
        <Table
          headers={["时间", "明细", "使用卡密", "金额", "变动后余额"]}
          rows={array(data, "ledger").map((l) => [
            date(l.createdAt),
            l.reason || l.kind,
            l.cardCode ? <code>{l.cardCode}</code> : "—",
            `${l.amountCents >= 0 ? "+" : ""}¥ ${money(l.amountCents)}`,
            `¥ ${money(l.balanceAfter)}`,
          ])}
        />
      </div>
    </>
  );
}
export function Plans({
  user,
  onRefresh,
}: {
  user: RecordData;
  onRefresh: () => void;
}) {
  const { data, error } = useData("/api/plans"),
    { data: channels } = useData("/api/payment-channels");
  const [selected, setSelected] = useState<RecordData | null>(null),
    [order, setOrder] = useState<RecordData | null>(null);
  return (
    <>
      <Header title="套餐购买" sub="按使用时间和流量选择适合自己的套餐。" />
      <ErrorNotice error={error} />
      <div className="summary">
        账户余额：<strong>¥ {money(user.balanceCents)}</strong>
      </div>
      <div className="grid3 plans mt24">
        {array(data, "plans").map((p) => (
          <div className="card" key={p.id}>
            <h3>{p.name}</h3>
            <div className="price num">
              ¥ {money(p.priceCents)}
              <span> / {p.days} 天</span>
            </div>
            <p className="muted">{gb(p.trafficBytes)} GB 总流量额度</p>
            <p className="mt8">每条规则上下行各 {p.rateMbps} Mbps</p>
            <ul className="features">
              <li>所有已启用线路均可选择</li>
              <li>每条线路独立配置</li>
              <li>余额 / 已开通的在线支付渠道</li>
              <li>
                {p.maxPurchasesPerUser
                  ? `每用户限购 ${p.maxPurchasesPerUser} 次，已购买 ${data?.purchasedCounts?.[p.id] || 0} 次`
                  : "购买次数不限"}
              </li>
            </ul>
            <Button
              primary
              className="block"
              disabled={
                !!(
                  p.maxPurchasesPerUser &&
                  (data?.purchasedCounts?.[p.id] || 0) >= p.maxPurchasesPerUser
                ) ||
                !!(data?.purchaseRequireVerifiedEmail && !user.emailVerifiedAt)
              }
              onClick={() => setSelected(p)}
            >
              选择套餐
            </Button>
          </div>
        ))}
      </div>
      {!array(data, "plans").length && <Empty>暂无上架套餐</Empty>}
      {data?.purchaseRequireVerifiedEmail && !user.emailVerifiedAt && (
        <Notice tone="orange">
          购买套餐前需验证邮箱，请前往“账户与邮箱验证”。
        </Notice>
      )}
      <div className="mt24">
        <Notice>
          本站增值中转服务不承诺 100%
          可用性。如您对稳定性要求较高，建议使用本站免费工具搭配自备服务器部署中转，或选择专业游戏加速器。
        </Notice>
        <div className="mt16">
          <Notice>
            再次购买条件：剩余时间少于30天，或剩余流量少于10GB。新套餐覆盖旧的剩余时间与流量，不叠加。
          </Notice>
        </div>
      </div>
      {selected && (
        <Modal
          title={"购买 " + selected.name}
          onClose={() => setSelected(null)}
        >
          <AsyncForm
            label="确认购买"
            onSubmit={async (f) => {
              const o = await post("/api/orders", {
                planId: selected.id,
                channelId: f.get("channelId"),
                requestId: requestID(),
                confirmReplace: true,
              });
              setOrder(o.order || o);
              setSelected(null);
              onRefresh();
            }}
          >
            <p className="price">¥ {money(selected.priceCents)}</p>
            <Select label="支付方式" name="channelId">
              <option value="balance">账户余额</option>
              {array(channels, "channels").map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
            </Select>
            <Check required label="我理解新套餐将覆盖旧的剩余时间和流量" />
          </AsyncForm>
        </Modal>
      )}
      {order && (
        <Modal title="订单已创建" onClose={() => setOrder(null)}>
          {order.channelId !== "balance" && order.amountCents > 0 ? (
            <PaymentReturn
              key={order.id}
              orderID={order.id}
              onPaid={onRefresh}
              onOrder={setOrder}
            />
          ) : (
            <Notice tone="orange">
              订单状态：
              {order.state === "paid"
                ? "已支付，权益已生效"
                : order.state === "fulfilled"
                  ? "已支付，权益已生效"
                  : "待支付"}
            </Notice>
          )}
          {order.paymentUrl && order.state === "pending" && (
            <a
              className="btn primary mt16"
              href={order.paymentUrl}
              rel="noreferrer"
              target="_blank"
            >
              前往支付
            </a>
          )}
          <p className="muted mt16">
            付款结果以服务端通知为准，可在订单页查看。
          </p>
        </Modal>
      )}
    </>
  );
}
export function Orders({
  admin = false,
  returnOrderID = null,
  onPaid,
}: {
  admin?: boolean;
  returnOrderID?: string | null;
  onPaid?: () => void;
}) {
  const { data, error, reload } = useData(
    admin ? "/api/admin/orders" : "/api/orders",
  );
  return (
    <>
      <Header
        title={admin ? "订单管理" : "我的订单"}
        sub="支付结果由服务端验签后确认。"
      >
        <Button onClick={reload}>刷新</Button>
      </Header>
      <ErrorNotice error={error} />
      {returnOrderID && (
        <PaymentReturn
          key={returnOrderID}
          orderID={returnOrderID}
          onPaid={() => {
            reload();
            onPaid?.();
          }}
        />
      )}
      <div className="card flush">
        <Table
          headers={[
            "订单号",
            ...(admin ? ["用户邮箱"] : []),
            "套餐",
            "金额",
            "支付状态",
            "时间",
            "操作",
          ]}
          rows={array(data, "orders").map((o) => [
            <span className="mono">{o.id}</span>,
            ...(admin ? [o.userEmail || o.userId] : []),
            o.plan?.name,
            `¥ ${money(o.amountCents)}`,
            <>
              <Badge tone={o.state === "paid_review" ? "red" : "orange"}>
                {(
                  {
                    paid: "已支付",
                    fulfilled: "已履约",
                    pending: "待支付",
                    paid_review: "待人工核对",
                    expired: "已过期",
                  } as Record<string, string>
                )[o.state] || o.state}
              </Badge>
              {o.reviewReason && <small>{o.reviewReason}</small>}
            </>,
            date(o.createdAt),
            o.paymentUrl && o.state === "pending" ? (
              <a
                className="btn"
                target="_blank"
                rel="noreferrer"
                href={o.paymentUrl}
              >
                继续支付
              </a>
            ) : (
              "—"
            ),
          ])}
        />
      </div>
    </>
  );
}
export function Routes({ user }: { user: RecordData }) {
  const { data, error, reload } = useData("/api/routes"),
    { data: rulesData, reload: reloadRules } = useData("/api/user/rules");
  const [selected, setSelected] = useState<RecordData | null>(null),
    [config, setConfig] = useState<RecordData | null>(null),
    [front, setFront] = useState<SSH>({
      host: "",
      port: 22,
      user: "root",
      password: "",
      fingerprint: "",
    }),
    [useFront, setUseFront] = useState(false),
    [message, setMessage] = useState("");
  const rules = array(rulesData, "rules");
  const userRate =
    rulesData?.userRateMbps ?? data?.userRateMbps ?? user.rateMbps;
  async function action(fn: () => Promise<unknown>) {
    try {
      await fn();
      reload();
      reloadRules();
      setMessage("");
    } catch (e) {
      setMessage((e as Error).message);
    }
  }
  useEffect(() => {
    const id = setInterval(() => {
      reloadRules();
      reload();
    }, 5000);
    return () => clearInterval(id);
  }, []);
  return (
    <>
      <Header
        title="选择线路，独立配置"
        sub="每条线路单独上传配置并创建，不会自动开通其他线路。"
      />
      <ErrorNotice error={error || message} />
      <div className="grid4">
        {[
          [
            "剩余时间",
            Math.max(0, Math.ceil((user.expiresAt - Date.now()) / 86400000)) +
              " 天",
          ],
          [
            "剩余流量",
            gb(Math.max(0, user.trafficTotal - user.trafficUsed)) + " GB",
          ],
          ["当前每规则限速", userRate + " Mbps"],
          ["已配置线路", rules.length + " 条"],
        ].map(([t, v]) => (
          <div className="card stat" key={t}>
            <div className="stat-top">{t}</div>
            <div className="stat-value">{v}</div>
          </div>
        ))}
      </div>
      <p className="muted mt16">
        每条中转规则独立限速，上下行分别计算；实际取用户限速与线路上限的较小值。
      </p>
      <div className="summary mt24 between">
        <span>同一账号所有线路必须中转同一个 MSBOOST 节点配置。</span>
        <Button
          disabled={rules.some(
            (rule) => relayNeedsRecovery(rule) || rule.stopStatus === "pending",
          )}
          onClick={() => {
            if (
              rules.some(
                (rule) =>
                  relayNeedsRecovery(rule) || rule.stopStatus === "pending",
              )
            )
              return;
            if (confirm("更换统一目标将撤销全部旧线路。请确认后逐条重新配置。"))
              void action(() =>
                post("/api/user/target/reset", { confirm: true }),
              );
          }}
        >
          更换统一目标
        </Button>
      </div>
      <div className="grid3 mt24">
        {array(data, "routes").map((route) => {
          const rule = rules.find((r) => r.routeId === route.id);
          return (
            <div className="card route-card" key={route.id}>
              <div className="between">
                <h3>{route.name}</h3>
                <Badge tone={route.online ? "green" : "orange"}>
                  {route.online ? "管理在线" : "管理离线"}
                </Badge>
              </div>
              <div className="route-facts">
                <div>
                  <small>此线路每规则有效限速</small>
                  <b>{rule?.effectiveRateMbps ?? route.rateMbps} Mbps</b>
                </div>
                <div>
                  <small>自备前置机</small>
                  <b>{route.requireFront ? "必须" : "可选"}</b>
                </div>
                <div>
                  <small>配置状态</small>
                  <b>
                    {rule ? relayStatus(rule, route.online).label : "尚未配置"}
                  </b>
                </div>
              </div>
              {rule && (
                <RelayStatusDetail rule={rule} routeOnline={route.online} />
              )}
              <div className="route-action">
                {rule ? (
                  <div className="actions">
                    <Button
                      primary
                      onClick={() =>
                        void action(async () =>
                          download(
                            await api(`/api/user/routes/${route.id}/config`),
                            route.name + ".json",
                          ),
                        )
                      }
                    >
                      从服务器下载
                    </Button>
                    <Button
                      disabled={
                        relayNeedsRecovery(rule) ||
                        ["revoking", "awaiting_front"].includes(rule.state)
                      }
                      onClick={() =>
                        !relayNeedsRecovery(rule) &&
                        void action(() =>
                          post(
                            `/api/user/routes/${route.id}/rules`,
                            { paused: rule.state !== "paused" },
                            "PATCH",
                          ),
                        )
                      }
                    >
                      {rule.state === "paused" ? "恢复" : "暂停"}
                    </Button>
                    <Button
                      disabled={relayNeedsRecovery(rule)}
                      onClick={() => {
                        if (relayNeedsRecovery(rule)) return;
                        if (
                          confirm(
                            "请求删除这条中转及服务器保存的配置？待节点停止确认前，端口及旧目标继续占用。",
                          )
                        )
                          void action(() =>
                            api(`/api/user/routes/${route.id}/rules`, {
                              method: "DELETE",
                            }),
                          );
                      }}
                    >
                      删除
                    </Button>
                  </div>
                ) : (
                  <Button
                    primary
                    onClick={() => {
                      setSelected(route);
                      setUseFront(!!route.requireFront);
                      setConfig(null);
                      setFront({
                        host: "",
                        port: 22,
                        user: "root",
                        password: "",
                        fingerprint: "",
                      });
                    }}
                  >
                    配置中转
                  </Button>
                )}
              </div>
            </div>
          );
        })}
      </div>
      {!array(data, "routes").length && (
        <Empty>暂无可用线路，请联系管理员。</Empty>
      )}
      {selected && (
        <Modal
          title={"配置中转 · " + selected.name}
          onClose={() => {
            setSelected(null);
            setConfig(null);
            setFront({ ...front, password: "" });
          }}
        >
          <AsyncForm
            label="确认配置此线路"
            onSubmit={async () => {
              if (!config) throw new Error("请上传此线路的节点配置");
              const preparedFront = useFront
                ? await prepareSSH(front, setFront)
                : undefined;
              await post(`/api/user/routes/${selected.id}/rules`, {
                config,
                requestId: requestID(),
                ...(preparedFront ? { front: preparedFront } : {}),
              });
              setSelected(null);
              setConfig(null);
              setFront({ ...front, password: "" });
              reloadRules();
            }}
          >
            <ConfigUpload onLoad={setConfig} />
            <Check
              checked={useFront}
              disabled={selected.requireFront}
              onChange={(e) => setUseFront(e.target.checked)}
              label={
                selected.requireFront
                  ? "此线路要求自备前置机"
                  : "使用自备前置机"
              }
            />
            {useFront && (
              <SSHFields
                value={front}
                onChange={setFront}
                title="前置机 IP 地址"
              />
            )}
            <Notice tone="orange">
              仅创建当前线路；套餐有效期间可以登录再次下载配置，到期删除服务器配置与本站转发。
            </Notice>
          </AsyncForm>
        </Modal>
      )}
    </>
  );
}
export function Traffic({ status = false }: { status?: boolean }) {
  const { data, error, reload } = useData(
    status ? "/api/routes" : "/api/user/traffic",
  );
  return (
    <>
      <Header
        title={status ? "线路状态" : "流量统计"}
        sub={
          status
            ? "显示最近一次 Agent 上报状态。"
            : "全部线路共用额度，多级链只在一个计费点计量。"
        }
      >
        <Button onClick={reload}>刷新</Button>
      </Header>
      <ErrorNotice error={error} />
      {status ? (
        <div className="grid3">
          {array(data, "routes").map((r) => (
            <div className="card" key={r.id}>
              <h3>{r.name}</h3>
              <div className="mt16">
                <Badge tone={r.online ? "green" : "red"}>
                  {r.online ? "在线" : "离线 / 无新上报"}
                </Badge>
              </div>
            </div>
          ))}
        </div>
      ) : (
        <div className="card">
          <div className="mini-grid">
            {[
              ["套餐总量", data?.trafficTotal],
              ["已用流量", data?.trafficUsed],
              [
                "剩余流量",
                Math.max(
                  0,
                  (data?.trafficTotal || 0) - (data?.trafficUsed || 0),
                ),
              ],
              [
                "本月累计",
                data?.months?.[new Date().toISOString().slice(0, 7)],
              ],
            ].map(([k, v]) => (
              <div key={k}>
                <small>{k}</small>
                <div className="stat-value">
                  {gb(Number(v) || 0)}
                  <span>GB</span>
                </div>
              </div>
            ))}
          </div>
          <div className="mt24">
            <Table
              headers={["月份", "累计流量"]}
              rows={Object.entries(data?.months || {})
                .sort(([a], [b]) => b.localeCompare(a))
                .map(([month, bytes]) => [month, gb(Number(bytes)) + " GB"])}
            />
          </div>
        </div>
      )}
    </>
  );
}
