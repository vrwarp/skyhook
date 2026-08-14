import js from '@eslint/js';
import tsParser from '@typescript-eslint/parser';
import tsPlugin from '@typescript-eslint/eslint-plugin';

export default [
  { ignores: ['dist/**', 'release/**', 'node_modules/**'] },
  js.configs.recommended,
  {
    files: ['**/*.ts'],
    languageOptions: {
      parser: tsParser,
      ecmaVersion: 2022,
      sourceType: 'module',
      parserOptions: { project: false },
      globals: {
        window: 'readonly', document: 'readonly', console: 'readonly',
        setTimeout: 'readonly', clearTimeout: 'readonly',
        setInterval: 'readonly', clearInterval: 'readonly',
        WebSocket: 'readonly', WebTransport: 'readonly', atob: 'readonly',
        Node: 'readonly', Element: 'readonly', HTMLElement: 'readonly',
        Buffer: 'readonly', process: 'readonly', URL: 'readonly',
        Response: 'readonly', Request: 'readonly', Uint8Array: 'readonly',
        self: 'readonly', caches: 'readonly', crypto: 'readonly', fetch: 'readonly',
        indexedDB: 'readonly', navigator: 'readonly', location: 'readonly',
        history: 'readonly', queueMicrotask: 'readonly', Worker: 'readonly',
        MessageEvent: 'readonly', Cache: 'readonly', CryptoKey: 'readonly',
        IDBDatabase: 'readonly', IDBObjectStore: 'readonly', IDBRequest: 'readonly',
        IDBTransactionMode: 'readonly', TextEncoder: 'readonly', TextDecoder: 'readonly',
        URLSearchParams: 'readonly', InputEvent: 'readonly', KeyboardEvent: 'readonly',
        HTMLElement: 'readonly', HTMLIFrameElement: 'readonly', HTMLImageElement: 'readonly',
        HTMLInputElement: 'readonly', HTMLFormElement: 'readonly', HTMLButtonElement: 'readonly',
        HTMLAnchorElement: 'readonly', HTMLOptionElement: 'readonly', HTMLTextAreaElement: 'readonly',
        HTMLDialogElement: 'readonly', HTMLDivElement: 'readonly', HTMLSpanElement: 'readonly',
        HTMLParagraphElement: 'readonly', Document: 'readonly', Event: 'readonly',
        ServiceWorkerGlobalScope: 'readonly', Transferable: 'readonly', atob: 'readonly',
      },
    },
    plugins: { '@typescript-eslint': tsPlugin },
    rules: {
      ...tsPlugin.configs.recommended.rules,
      'no-undef': 'off',
      'no-unused-vars': 'off',
      '@typescript-eslint/no-unused-vars': ['error', { argsIgnorePattern: '^_', varsIgnorePattern: '^_' }],
      '@typescript-eslint/no-explicit-any': 'error',
      'no-empty': ['error', { allowEmptyCatch: true }],
      eqeqeq: ['error', 'smart'],
    },
  },
];
