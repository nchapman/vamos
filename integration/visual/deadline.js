// page.evaluate has no Playwright timeout. Bound the await so its caller can
// close the browser and interrupt a stuck renderer/font promise in finally.
export async function withDeadline(promise, milliseconds, label) {
  let timer;
  try {
    return await Promise.race([promise, new Promise((_, reject) => {
      timer = setTimeout(() => reject(new Error(`${label} exceeded ${milliseconds}ms`)), milliseconds);
    })]);
  } finally {
    clearTimeout(timer);
  }
}
