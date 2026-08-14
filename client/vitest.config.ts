import { defineConfig } from 'vitest/config';

// Patcher and echo tests need a real DOM: their whole job is building one. The
// rest are happier in plain node. Vitest 4 dropped environmentMatchGlobs, so
// that split is now two projects over the same test directory.
export default defineConfig({
  test: {
    projects: [
      {
        test: {
          name: 'node',
          environment: 'node',
          include: ['test/**/*.test.ts'],
          exclude: ['**/node_modules/**', 'test/**/*.dom.test.ts'],
          testTimeout: 20000,
        },
      },
      {
        test: {
          name: 'dom',
          environment: 'jsdom',
          include: ['test/**/*.dom.test.ts'],
          exclude: ['**/node_modules/**'],
          testTimeout: 20000,
        },
      },
    ],
  },
});
