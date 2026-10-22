---
name: test-first
description: Build a feature or fix a bug test first, red then green, at a seam the project already tests. Use when you are about to change behaviour and the project has tests, or when the person asks for tests.
license: MIT
---

# Test first

1. **Find where the project tests this kind of behaviour.** Read the tests
   beside the code you will change and copy their shape: the same package or
   folder, the same helpers, the same naming.
2. **Write one failing test** that states the behaviour you want, in the
   terms of the request ("a reminder at 17:00 fires once"), not of the code
   you expect to write.
3. **Run it and watch it fail** for the right reason. A test that fails
   because it does not compile, or because of a typo, proves nothing yet.
4. **Make it pass** with the smallest change that does.
5. **Run the whole suite**, not only your test, and the project's linter.
6. **Repeat** for the next behaviour. When the behaviour is done, tidy the
   code with the tests green.

For a bug: the first test reproduces the bug as the person reported it. If
you cannot make it fail, you have not found the bug yet; say so rather than
changing code.

When you report, name the tests you added and what each one proves, and the
command that ran them.
