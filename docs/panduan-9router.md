# Panduan Integrasi AgentRouter Spoof Proxy dengan 9Router

**Bahasa Indonesia**

> Dokumen ini menjelaskan cara menggunakan **AgentRouter Spoof Proxy** sebagai *middleware* di belakang **9Router** untuk menembus (*bypass*) limitasi WAF/cookie dari **agentrouter.org**.

---

## Arsitektur

```
┌──────────────┐     ┌────────────────────────┐     ┌──────────────────────┐     ┌──────────────────┐
│  Client AI   │ ──→ │       9Router          │ ──→ │  agentrouter-proxy   │ ──→ │ agentrouter.org  │
│  (opencode,  │     │  (load balancer/router) │     │  :8318 (spoof proxy) │     │  (upstream)      │
│   Cursor,    │     │                        │     │                      │     │                  │
│   dll.)      │     │                        │     │                      │     │                  │
└──────────────┘     └────────────────────────┘     └──────────────────────┘     └──────────────────┘
```

**Alur:**
1. Client AI (opencode, Cursor, dll) mengirim request ke 9Router
2. 9Router meneruskan ke agentrouter-proxy (`localhost:8318`)
3. agentrouter-proxy menyisipkan header palsu (spoof headers) dan cookie WAF
4. Request diteruskan ke `agentrouter.org` (upstream asli)
5. Response streaming SSE dikembalikan ke client

---

## Kenapa Perlu Middleware?

**agentrouter.org** menggunakan:
- **WAF (Web Application Firewall)** dari Alibaba Cloud — butuh cookie `acw_tc` yang di-refresh berkala
- **Deteksi User-Agent** — hanya klien resmi (Claude Code) yang dilayani
- **Rate limiting** per channel

**AgentRouter Spoof Proxy** menangani semua itu:
- Memperbarui cookie WAF setiap 3 menit
- Menyamar sebagai Claude Code CLI
- Retry otomatis jika kena blokir WAF
- Circuit breaker jika upstream bermasalah

Dengan menaruh proxy ini di **belakang 9Router**, kamu bisa:
- Memakai satu API key untuk banyak model
- Load balancing antar model
- Fallback ke provider lain jika agentrouter down

---

## Persyaratan

| Komponen | Minimal |
|----------|---------|
| Docker | `docker --version` |
| Node.js | 22+ (jika tanpa Docker) |
| 9Router | Sudah terinstall dan berjalan |
| API Key | Dari akun agentrouter.org |

---

## Langkah 1 — Deploy AgentRouter Spoof Proxy

### Opsi A — Docker Compose (Rekomendasi)

```bash
git clone https://github.com/trefeon/agentrouter-spoof-proxy.git
cd agentrouter-spoof-proxy
cp .env.example .env
docker compose up -d --build
```

### Opsi B — Node.js Langsung (Tanpa Docker)

```bash
git clone https://github.com/trefeon/agentrouter-spoof-proxy.git
cd agentrouter-spoof-proxy
cp .env.example .env
node proxy.mjs
```

> Jalankan di terminal/screen/tmux agar tetap hidup.

---

## Langkah 2 — Verifikasi Proxy

Tunggu beberapa detik sampai WAF warmup selesai, lalu cek:

```bash
curl http://localhost:8318/health
```

Hasil yang diharapkan:

```json
{
  "ok": true,
  "wafCookie": true,
  "circuitOpen": false,
  "modelSource": "static",
  "availableModels": 5
}
```

> Jika `wafCookie: false`, tunggu 5 detik dan coba lagi. Proxy sedang mengambil cookie WAF dari agentrouter.org.

---

## Langkah 3 — Hubungkan ke Jaringan Docker 9Router

Agar 9Router bisa berkomunikasi dengan proxy, keduanya harus berada di **jaringan Docker yang sama**.

### Cek nama jaringan 9Router:

```bash
docker network ls
```

Cari jaringan 9Router (biasanya `9router-net` atau `9router_default`).

### Sambungkan proxy ke jaringan tersebut:

```bash
docker network connect 9router-net agentrouter-proxy
```

### Atau gunakan `docker-compose.override.yml` (otomatis tergabung dengan `docker-compose.yml`):

```bash
cp docker-compose.override.yml.example docker-compose.override.yml
```

Edit `docker-compose.override.yml` dan sesuaikan nama jaringan 9Router kamu:

```yaml
services:
  agentrouter-proxy:
    networks:
      - proxy-net            # nama bebas, terserah kamu

networks:
  proxy-net:
    external: true
    name: 9router-net        # ganti dengan nama jaringan 9Router kamu
```

> Cek nama jaringan 9Router dengan `docker network ls`.

Lalu restart:

```bash
docker compose up -d
```

---

## Langkah 4 — Konfigurasi 9Router

### 4.1 — Tambah Provider Baru

1. Buka dashboard 9Router
2. Masuk ke menu **Providers**
3. Klik **Add Provider**
4. Pilih **Add OpenAI Compatible** (yang *Custom*)

### 4.2 — Isi Konfigurasi Provider

Isi form dengan:

| Field | Isi |
|-------|-----|
| **Name** | Terserah, misal: `AgentRouter` |
| **Prefix** | `AG` (singkatan AgentRouter — untuk prefix model ID) |
| **API Type** | `chat completions` |
| **Base URL** | `http://localhost:8318/v1` |

> **Catatan:** Base URL pakai `localhost:8318` karena proxy berjalan di host yang sama. Jika proxy di server lain, ganti `localhost` dengan IP server proxy.

### 4.3 — Import Model dari /models

1. Setelah provider tersimpan, cari tombol **Import from /models** (atau sejenisnya)
2. Klik tombol tersebut
3. 9Router akan memanggil `http://localhost:8318/v1/models` dan otomatis mengambil daftar model

Model yang akan muncul:
- `claude-opus-4-6`
- `claude-opus-4-7`
- `claude-opus-4-8`
- `glm-5.2`
- `gpt-5.5` (akan 403 — tidak bisa dipakai)

Jika mengaktifkan **Model Auto-Discovery** (dengan `AR_API_KEY`), model yang tampil akan sesuai dengan akun agentrouter.org kamu.

### 4.4 — Tambah API Key ke Provider

> **PENTING:** API key agentrouter.org **hanya** dimasukkan ke 9Router, **bukan** ke proxy.

1. Di halaman provider yang baru dibuat, cari bagian **API Keys**
2. Klik **Add API Key**
3. Masukkan API Key dari akun agentrouter.org kamu
4. Simpan

9Router akan menggunakan API key ini saat mengirim request ke proxy. Proxy akan meneruskannya ke agentrouter.org tanpa perlu tahu API key-nya.

---

## Langkah 5 — Verifikasi Integrasi

### Test dari 9Router:

```bash
curl http://localhost:9ROUTER_PORT/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer API_KEY_9ROUTER" \
  -d '{
    "model": "AG-claude-opus-4-8",
    "messages": [{"role": "user", "content": "Halo, apa kabar?"}],
    "stream": true
  }'
```

> Ganti `9ROUTER_PORT` dengan port 9Router kamu, dan `API_KEY_9ROUTER` dengan API key dari 9Router.
>
> Prefix `AG-` harus ditambahkan karena 9Router menggunakan prefix untuk routing ke provider yang benar.

### Test Langsung ke Proxy (Lewati 9Router):

```bash
curl http://localhost:8318/v1/messages \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer API_KEY_AGENTROUTER" \
  -d '{
    "model": "claude-opus-4-8",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Halo"}],
    "stream": true
  }'
```

---

## Langkah 6 — Konfigurasi opencode / Cursor

### opencode

Edit `~/.config/opencode/opencode.jsonc`, tambahkan provider:

```jsonc
"provider": {
  "agentrouter": {
    "npm": "@ai-sdk/openai-compatible",
    "name": "AgentRouter via 9Router",
    "options": {
      "baseURL": "http://localhost:9ROUTER_PORT/v1"
    },
    "models": {
      "claude-opus-4-8": {
        "id": "AG-claude-opus-4-8",
        "name": "Claude Opus 4.8",
        "vision": true, "reasoning": true, "tool_call": true,
        "cost": { "input": 5, "output": 25, "cache_read": 0.5, "cache_write": 6.25 },
        "limit": { "context": 1000000, "output": 128000 }
      }
    }
  }
}
```

Lalu di opencode TUI ketik:

```
/connect agentrouter
```

Masukkan API key 9Router (bukan API key agentrouter).

### Cursor

1. Settings → Models → Add model
2. Model ID: `AG-claude-opus-4-8`
3. API Endpoint: `http://localhost:9ROUTER_PORT/v1`
4. API Key: API key 9Router

---

## Model yang Tersedia

| Model ID (di 9Router) | Deskripsi | Context | Output |
|-----------------------|-----------|---------|--------|
| `AG-claude-opus-4-6` | Claude Opus 4.6 | 1M | 128k |
| `AG-claude-opus-4-7` | Claude Opus 4.7 | 1M | 128k |
| `AG-claude-opus-4-8` | Claude Opus 4.8 (irit token ~35%) | 1M | 128k |
| `AG-glm-5.2` | GLM 5.2 (lebih murah) | 1M | 131k |
| `AG-gpt-5.5` | ❌ Selalu 403 — tidak bisa dipakai | — | — |

---

## Catatan Penting

| Masalah | Penyebab | Solusi |
|---------|----------|--------|
| `wafCookie: false` | Proxy gagal warmup | Cek koneksi ke `agentrouter.org`, tunggu beberapa detik |
| `circuitOpen: true` | 5+ gagal berurutan | Tunggu backoff, cek ketersediaan agentrouter.org |
| `NoChannelError` (503) | Tidak ada channel untuk model itu | Coba model lain atau retry |
| 403 pada request | WAF block atau kuota habis | WAF: di-retry otomatis. Kuota: ganti model |
| 502/504 | Timeout dari upstream | Cek jaringan, naikkan `REQUEST_TIMEOUT_MS` |
| 429 | Rate limit TPM | Tunggu dan retry |

---

## Tips

- **API key agentrouter cukup dimasukkan di 9Router** — proxy tidak perlu tahu API key
- **Proxy bisa dipakai tanpa 9Router** — langsung `curl` ke `localhost:8318/v1/messages`
- **Jika 9Router dan proxy di server berbeda**, ganti `localhost` dengan IP server proxy
- **Aktifkan `LOG_LEVEL=debug`** di `.env` jika ingin melihat log detail (chunk, timing, dll)
- **Gunakan `AR_API_KEY`** untuk auto-discovery model, agar model selalu up-to-date
