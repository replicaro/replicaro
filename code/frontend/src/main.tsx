import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './index.css'
import App from './App.tsx'
import { getSettings } from './services/api.ts'
import { applyThemePreference } from './theme.ts'
import { activateLocale } from './i18n.ts'

// This is the one place the app loads a language catalog. Changing the
// language on the System page saves the settings and reloads the page, which
// comes back through here, so there is no second loading path to keep in step.
async function start() {
  let failedLocale: string | undefined
  try {
    const settings = await getSettings()
    applyThemePreference(settings.theme)
    // Download the saved language's catalog before the first render so the UI
    // doesn't flash English first. English needs no request. If the catalog
    // can't be loaded, render in English rather than holding the UI back, and
    // let App say so once it is on screen.
    if (await activateLocale(settings.effectiveLocale) === 'failed') {
      failedLocale = settings.effectiveLocale
      await activateLocale('en')
    }
  } catch {
    applyThemePreference('dark')
    await activateLocale('en')
  }

  createRoot(document.getElementById('root')!).render(
    <StrictMode>
      <App failedLocale={failedLocale} />
    </StrictMode>,
  )
}

void start()
