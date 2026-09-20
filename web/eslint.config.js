import js from '@eslint/js'
import ts from 'typescript-eslint'
import vue from 'eslint-plugin-vue'
import vueParser from 'vue-eslint-parser'
import prettier from 'eslint-config-prettier'
import globals from 'globals'

export default ts.config(
  {
    ignores: [
      '**/dist/**',
      '**/node_modules/**',
      '**/coverage/**',
      // Generated from api/openapi.yaml by `pnpm gen`; never hand-edited.
      'packages/api/src/schema.d.ts',
    ],
  },
  js.configs.recommended,
  ...ts.configs.recommended,
  ...vue.configs['flat/recommended'],
  {
    languageOptions: {
      ecmaVersion: 2023,
      sourceType: 'module',
      globals: { ...globals.browser, ...globals.node },
    },
  },
  {
    files: ['**/*.vue'],
    languageOptions: {
      parser: vueParser,
      parserOptions: {
        parser: ts.parser,
        extraFileExtensions: ['.vue'],
        sourceType: 'module',
      },
    },
    rules: {
      // Page components are named after the route they serve, which is a single
      // word often enough (`SettingsPage` is fine, `Button` is not exported).
      'vue/multi-word-component-names': 'off',
      'vue/require-default-prop': 'off',
      'vue/attribute-hyphenation': ['error', 'always'],
      'vue/no-v-html': 'error',
    },
  },
  {
    files: ['**/*.ts', '**/*.vue'],
    rules: {
      // TypeScript already resolves globals and DOM lib types; `no-undef` only
      // produces false positives on type-only identifiers like `RequestInfo`.
      'no-undef': 'off',
    },
  },
  {
    rules: {
      '@typescript-eslint/no-unused-vars': [
        'error',
        { argsIgnorePattern: '^_', varsIgnorePattern: '^_', caughtErrorsIgnorePattern: '^_' },
      ],
      '@typescript-eslint/consistent-type-imports': [
        'error',
        { prefer: 'type-imports', fixStyle: 'inline-type-imports' },
      ],
      'no-console': ['warn', { allow: ['warn', 'error'] }],
      eqeqeq: ['error', 'smart'],
    },
  },
  {
    files: ['**/*.config.{ts,js}', '**/test/**', '**/*.test.ts', '**/*.spec.ts'],
    rules: {
      'no-console': 'off',
      '@typescript-eslint/no-explicit-any': 'off',
      // Test fixtures define throwaway render components inline.
      'vue/one-component-per-file': 'off',
      'vue/multi-word-component-names': 'off',
    },
  },
  prettier,
)
