FROM node:22-alpine

WORKDIR /app
COPY proxy.mjs .

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8318/health || exit 1

EXPOSE 8318

CMD ["node", "proxy.mjs"]
