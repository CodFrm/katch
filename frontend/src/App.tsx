import { Navigate, Route, Routes } from 'react-router-dom'

import { AdminPage } from '@/routes/admin-page'
import { PublicPage } from '@/routes/public-page'

/**
 * 两个面的路由：前台公开，后台要密钥。
 *
 * Router 本身装在 main.tsx 上而不是这里，用例才能用 MemoryRouter 指定起始路径。
 * 认不出来的路径回前台而不是给一页 404：后端已经把 /assets/ 与 /api/ 之外的未命中
 * 路径回落到 index.html 了，走到这里的都是站内链接敲错，回首页比一块空白有用。
 */
export default function App() {
  return (
    <Routes>
      <Route path="/" element={<PublicPage />} />
      <Route path="/admin/*" element={<AdminPage />} />
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  )
}
