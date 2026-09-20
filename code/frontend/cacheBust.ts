import { createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

export const cacheBustedPublicAssets = [
  'public/favicon-teal-64.png',
  'public/favicon.ico',
  'public/replicaro-wordmark-teal.png',
  'public/replicaro-wordmark-white.png',
] as const

export function contentCacheBust(
  frontendRoot: string,
  assets: readonly string[] = cacheBustedPublicAssets,
) {
  const digest = createHash('sha256')
  for (const asset of [...assets].sort()) {
    digest.update(asset)
    digest.update('\0')
    digest.update(readFileSync(resolve(frontendRoot, asset)))
    digest.update('\0')
  }
  return digest.digest('hex').slice(0, 20)
}
