import type { Locator, Page } from "@playwright/test";

export async function dropFile(page: Page, zone: Locator, file: { name: string; mimeType: string; buffer: Buffer }) {
  const dataTransfer = await page.evaluateHandle(({ name, type, content }) => {
    const transfer = new DataTransfer();
    const data = Uint8Array.from(atob(content), (character) => character.charCodeAt(0));
    transfer.items.add(new File([data], name, { type }));
    return transfer;
  }, { name: file.name, type: file.mimeType, content: file.buffer.toString("base64") });
  try {
    await zone.dispatchEvent("dragenter", { dataTransfer });
    await zone.dispatchEvent("dragover", { dataTransfer });
    await zone.dispatchEvent("drop", { dataTransfer });
  } finally {
    await dataTransfer.dispose();
  }
}
