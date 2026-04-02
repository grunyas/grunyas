FROM node:22-alpine AS builder
WORKDIR /app
COPY package.json package-lock.json* tsconfig.json ./
RUN npm install
COPY src/ ./src/
RUN npm run build

FROM node:22-alpine
WORKDIR /app
COPY --from=builder /app/package.json /app/package-lock.json* ./
COPY --from=builder /app/node_modules/ ./node_modules/
COPY --from=builder /app/dist/ ./dist/
RUN mkdir -p /app/results
CMD ["node", "dist/index.js"]
