import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App'
import { applyTheme, getInitialPref } from './lib/theme'
import './index.css'

// 首帧前把主题钉住：index.html 的内联脚本已设过 data-theme，
// 这里补上 data-pref 并处理 system 偏好，避免首屏闪一下
applyTheme(getInitialPref())

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
)
