import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './index.css'
import App from './App.tsx'
import { getSettings } from './services/api.ts'
import { applyThemePreference } from './theme.ts'

async function start() {
  try {
    const settings = await getSettings()
    applyThemePreference(settings.theme)
  } catch {
    applyThemePreference('dark')
  }

  createRoot(document.getElementById('root')!).render(
    <StrictMode>
      <App />
    </StrictMode>,
  )
}

void start()
