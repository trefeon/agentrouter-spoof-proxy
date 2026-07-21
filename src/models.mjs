import { UPSTREAM_MODULE, TARGET_HOST_VAL, TARGET_PORT_INT, AGENT, MODELS_CSV_VAL, AR_API_KEY_VAL } from "./config.mjs";
import { log } from "./logger.mjs";

const STATIC_MODELS = MODELS_CSV_VAL.split(",").map((id) => ({
  id: id.trim(),
  object: "model",
  created: 1626777600,
  owned_by: "agentrouter",
}));

let modelsList = [...STATIC_MODELS];
let modelSource = "static";

export function getModelsList() {
  return modelsList;
}

export function getModelSource() {
  return modelSource;
}

export async function fetchModels() {
  if (!AR_API_KEY_VAL) return;
  const ts = new Date().toISOString();
  try {
    const data = await new Promise((resolve, reject) => {
      const req = UPSTREAM_MODULE.request(
        {
          hostname: TARGET_HOST_VAL,
          port: TARGET_PORT_INT,
          path: "/v1/models",
          method: "GET",
          headers: {
            Authorization: `Bearer ${AR_API_KEY_VAL}`,
            "User-Agent": "agentrouter-spoof-proxy/1.0",
            Accept: "application/json",
          },
          agent: AGENT,
          rejectUnauthorized: true,
          timeout: 15000,
        },
        (res) => {
          const chunks = [];
          res.on("data", (c) => chunks.push(c));
          res.on("end", () => {
            const raw = Buffer.concat(chunks);
            if (res.statusCode === 200) {
              try { resolve(JSON.parse(raw)); }
              catch { reject(new Error("bad json")); }
            } else {
              reject(new Error(`status ${res.statusCode}`));
            }
          });
        }
      );
      req.on("error", reject);
      req.on("timeout", () => { req.destroy(); reject(new Error("timeout")); });
      req.end();
    });

    if (data?.data && Array.isArray(data.data)) {
      modelsList = data.data.map((m) => ({
        id: m.id,
        object: "model",
        created: m.created || 1626777600,
        owned_by: m.owned_by || "agentrouter",
      }));
      modelSource = "dynamic";
      log(ts, `DISCOVERED ${modelsList.length} models from upstream`);
    }
  } catch (e) {
    log(ts, `Model discovery failed: ${e.message}, using static list`);
    modelSource = "static";
    modelsList = [...STATIC_MODELS];
  }
}
