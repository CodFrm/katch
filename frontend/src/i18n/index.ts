import i18n from 'i18next'
import LanguageDetector from 'i18next-browser-languagedetector'
import { initReactI18next } from 'react-i18next'

import en from './locales/en.json'
import zhCN from './locales/zh-CN.json'

// fallbackLng 用 zh-CN：katch 的主要使用者是中文用户，检测不出语言时给中文比
// 给英文更可能是对的。新增语言只需在这里注册，组件里不必改动。
void i18n
  .use(LanguageDetector)
  .use(initReactI18next)
  .init({
    resources: {
      en: { translation: en },
      'zh-CN': { translation: zhCN },
    },
    fallbackLng: 'zh-CN',
    interpolation: { escapeValue: false },
  })

export default i18n
