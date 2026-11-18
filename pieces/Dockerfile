# The pieces runner: Activepieces piece packages plus server.mjs.
#   docker build -t apex-pieces pieces/
# Add pieces that are not in package.json without editing it:
#   docker build --build-arg EXTRA_PIECES="xero quickbooks" -t apex-pieces pieces/
FROM node:22-slim
ENV NODE_ENV=production PIECES_HOST=0.0.0.0 PIECES_PORT=7391
WORKDIR /runner
COPY package.json ./
RUN npm install --omit=dev --no-audit --no-fund
ARG EXTRA_PIECES=""
RUN if [ -n "$EXTRA_PIECES" ]; then \
      npm install --omit=dev --no-audit --no-fund $(for p in $EXTRA_PIECES; do printf '@activepieces/piece-%s ' "$p"; done); \
    fi
COPY server.mjs ./
USER node
EXPOSE 7391
HEALTHCHECK --interval=15s --timeout=3s --start-period=10s --retries=5 \
  CMD node -e "fetch('http://127.0.0.1:'+process.env.PIECES_PORT+'/healthz').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"
CMD ["node", "server.mjs"]
