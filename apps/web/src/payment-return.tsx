import { useEffect, useRef, useState } from "react";
import { api, RecordData } from "./api";
import { Button, ErrorNotice, money, Notice } from "./ui";

// This is only an order selector. No payment status or signature from the
// browser URL is trusted; the authenticated order API owns the result.
export function paymentReturnOrder(search = location.search): string | null {
  const values = new URLSearchParams(search).getAll("order");
  return values.length === 1 && /^[A-Za-z0-9_-]{1,128}$/.test(values[0])
    ? values[0]
    : null;
}

export function PaymentReturn({
  orderID,
  onPaid,
  onOrder,
}: {
  orderID: string;
  onPaid?: () => void;
  onOrder?: (order: RecordData) => void;
}) {
  const [receivedOrder, setOrder] = useState<RecordData | null>(null),
    [error, setError] = useState(""),
    [busy, setBusy] = useState(false),
    [stopped, setStopped] = useState(false),
    [revision, setRevision] = useState(0);
  const order = receivedOrder?.id === orderID ? receivedOrder : null;
  const paidHandler = useRef(onPaid),
    notifiedOrderID = useRef<string | null>(null);
  const orderHandler = useRef(onOrder);
  paidHandler.current = onPaid;
  orderHandler.current = onOrder;
  useEffect(() => {
    let active = true,
      timer: ReturnType<typeof setTimeout> | undefined,
      controller: AbortController | undefined;
    let attempts = 0;
    setError("");
    setStopped(false);
    async function check() {
      setBusy(true);
      controller = new AbortController();
      const requestController = controller;
      const timeout = setTimeout(() => requestController.abort(), 10000);
      try {
        const result = await api<RecordData>(
          `/api/orders/${encodeURIComponent(orderID)}`,
          { signal: requestController.signal },
        );
        if (!active) return;
        if (result.id !== orderID)
          throw new Error("订单响应不匹配，请重新刷新");
        setOrder(result);
        orderHandler.current?.(result);
        setError("");
        if (result.state === "paid" && notifiedOrderID.current !== orderID) {
          notifiedOrderID.current = orderID;
          paidHandler.current?.();
        }
        // At most one minute of polling per entry/manual refresh; stop on
        // terminal state or any error rather than retrying forever.
        if (result.state === "pending" && ++attempts < 13) {
          timer = setTimeout(() => void check(), 5000);
        } else if (result.state === "pending") {
          setStopped(true);
        }
      } catch (e) {
        if (active) {
          setOrder(null);
          setError(
            requestController.signal.aborted
              ? "订单查询超时，请手动刷新"
              : (e as Error).message,
          );
        }
      } finally {
        clearTimeout(timeout);
        if (active) setBusy(false);
      }
    }
    void check();
    return () => {
      active = false;
      clearTimeout(timer);
      controller?.abort();
    };
  }, [orderID, revision]);
  const labels: Record<string, string> = {
    pending: "等待付款通知",
    paid: "已支付，权益已生效",
    paid_review: "款项已收到，待人工核对",
    expired: "订单已过期",
  };
  return (
    <section
      className="card"
      style={{ marginBottom: 24 }}
      aria-label="支付返回结果"
    >
      <h2>支付返回结果</h2>
      <p className="muted mt8">
        返回支付页面不代表付款成功，以下结果仅来自本人订单的服务端记录。
      </p>
      <p className="mono mt16" style={{ overflowWrap: "anywhere" }}>
        订单号：{orderID}
      </p>
      <ErrorNotice error={error} />
      {order ? (
        <>
          <p className="mt16">
            {order.plan?.name} · ¥ {money(order.amountCents)}
          </p>
          <Notice tone={order.state === "paid" ? "green" : "orange"}>
            {labels[order.state] || "状态待核对"}
            {order.reviewReason && <p>{order.reviewReason}</p>}
          </Notice>
          {order.state !== "paid" && (
            <p className="muted mt8">
              若已扣款，请勿重复付款。可以刷新等待通知，或在工单中提供订单号请管理员核对。
            </p>
          )}
        </>
      ) : (
        !error && <p className="mt16">正在查询付款状态…</p>
      )}
      {stopped && (
        <p className="muted mt8">
          本轮自动查询已结束，可手动刷新；这不会主动向支付平台查单。
        </p>
      )}
      <Button
        className="mt16"
        disabled={busy}
        onClick={() => setRevision((v) => v + 1)}
      >
        {busy ? "正在查询…" : "刷新付款状态"}
      </Button>
    </section>
  );
}
