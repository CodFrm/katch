import js from '@eslint/js'
import { defineConfig, globalIgnores } from 'eslint/config'
import i18next from 'eslint-plugin-i18next'
import prettier from 'eslint-plugin-prettier/recommended'
import reactHooks from 'eslint-plugin-react-hooks'
import reactRefresh from 'eslint-plugin-react-refresh'
import tseslint from 'typescript-eslint'

export default defineConfig([
  globalIgnores(['dist', 'node_modules', 'coverage', 'src/components/ui']),
  js.configs.recommended,
  tseslint.configs.recommended,
  i18next.configs['flat/recommended'],
  {
    files: ['**/*.{ts,tsx}'],
    plugins: {
      'react-hooks': reactHooks,
      'react-refresh': reactRefresh,
    },
    rules: {
      ...reactHooks.configs.recommended.rules,
      // exhaustive-deps 不在 react-hooks v7 的 recommended 里，但它是真正的正确性
      // 规则（漏依赖 = 读到过期闭包），显式打开。
      'react-hooks/exhaustive-deps': 'warn',
      'react-refresh/only-export-components': 'off',
      '@typescript-eslint/no-unused-vars': [
        'warn',
        { argsIgnorePattern: '^_', varsIgnorePattern: '^_' },
      ],

      // ── i18n 闸 ───────────────────────────────────────────────────
      // 界面文案一律走 t()，否则新语言只能翻到一半，而且漏掉的那部分没人发现。
      // 见 docs/frontend.md#i18n
      'i18next/no-literal-string': [
        'error',
        {
          mode: 'jsx-only',
          'jsx-components': {
            exclude: ['Trans', 'code', 'pre', 'script', 'style'],
          },
          'jsx-attributes': {
            include: ['aria-label', 'aria-description', 'title', 'placeholder', 'alt'],
          },
          words: {
            // 纯标点、数字、分隔符不算文案；镜像站的路径样例（docker.io 等）
            // 是机器面向的字面量，翻译它们反而是错的。
            exclude: ['^[\\s\\p{P}\\p{S}\\d]+$', '^[a-z0-9.-]+\\.[a-z]{2,}$'],
          },
        },
      ],
    },
  },
  // shadcn 生成的 ui 组件是上游代码，不参与 i18n 闸和风格规则；
  // 它们已在 globalIgnores 里，这里只留说明，避免有人误以为是漏配。
  prettier,
])
