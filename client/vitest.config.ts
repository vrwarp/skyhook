import { defineConfig } from 'vitest/config';

export default defineConfig({
  test: {
    environment: 'node',
    include: ['test/**/*.test.ts'],
    // Patcher and echo tests need a real DOM: their whole job is building one.
    environmentMatchGlobs: [['test/**/*.dom.test.ts', 'jsdom']],
    testTimeout: 20000,
  },
});
