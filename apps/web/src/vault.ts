// Free configurations are encrypted on this browser and scoped to account+task.
const NAME = "msboost-production-vault-v1";
type Envelope = { iv: Uint8Array; cipher: ArrayBuffer; name: string };
function database(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const r = indexedDB.open(NAME, 1);
    r.onupgradeneeded = () => {
      r.result.createObjectStore("keys");
      r.result.createObjectStore("files");
    };
    r.onsuccess = () => resolve(r.result);
    r.onerror = () => reject(r.error);
  });
}
async function run<T>(
  store: string,
  mode: IDBTransactionMode,
  op: (s: IDBObjectStore) => IDBRequest<T>,
): Promise<T> {
  const db = await database();
  return new Promise((resolve, reject) => {
    const tx = db.transaction(store, mode);
    const req = op(tx.objectStore(store));
    let result: T;
    req.onsuccess = () => {
      result = req.result;
    };
    tx.oncomplete = () => {
      db.close();
      resolve(result);
    };
    tx.onerror = () => {
      db.close();
      reject(tx.error);
    };
    tx.onabort = () => {
      db.close();
      reject(tx.error);
    };
  });
}
const pendingKeys = new Map<string, Promise<CryptoKey>>();
function key(owner: string): Promise<CryptoKey> {
  if (pendingKeys.has(owner)) return pendingKeys.get(owner)!;
  const promise = (async () => {
    const existing = await run<CryptoKey | undefined>("keys", "readonly", (s) =>
      s.get(owner),
    );
    if (existing) return existing;
    const generated = await crypto.subtle.generateKey(
      { name: "AES-GCM", length: 256 },
      false,
      ["encrypt", "decrypt"],
    );
    try {
      await run("keys", "readwrite", (s) => s.add(generated, owner));
      return generated;
    } catch (error) {
      const winner = await run<CryptoKey | undefined>("keys", "readonly", (s) =>
        s.get(owner),
      );
      if (winner) return winner;
      throw error;
    }
  })();
  pendingKeys.set(owner, promise);
  promise.then(
    () => pendingKeys.delete(owner),
    () => pendingKeys.delete(owner),
  );
  return promise;
}
export async function saveLocal(
  owner: string,
  id: string,
  data: unknown,
  name: string,
) {
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const k = await key(owner);
  const cipher = await crypto.subtle.encrypt(
    {
      name: "AES-GCM",
      iv,
      additionalData: new TextEncoder().encode(owner + ":" + id),
    },
    k,
    new TextEncoder().encode(JSON.stringify(data)),
  );
  await run("files", "readwrite", (s) =>
    s.put({ iv, cipher, name }, owner + ":" + id),
  );
}
export async function getLocal(
  owner: string,
  id: string,
): Promise<{ data: unknown; name: string } | null> {
  const e = await run<Envelope | undefined>("files", "readonly", (s) =>
    s.get(owner + ":" + id),
  );
  if (!e) return null;
  const clear = await crypto.subtle.decrypt(
    {
      name: "AES-GCM",
      iv: e.iv as BufferSource,
      additionalData: new TextEncoder().encode(owner + ":" + id),
    },
    await key(owner),
    e.cipher,
  );
  return { data: JSON.parse(new TextDecoder().decode(clear)), name: e.name };
}
export const deleteLocal = (owner: string, id: string) =>
  run("files", "readwrite", (s) => s.delete(owner + ":" + id));
