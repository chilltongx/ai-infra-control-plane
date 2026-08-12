async page => {
const errors = [];
page.on('console', message => {
  if (message.type() === 'error') errors.push(`console: ${message.text()}`);
});
page.on('pageerror', error => errors.push(`pageerror: ${error.message}`));

await page.goto('http://127.0.0.1:18080');
await page.getByRole('heading', { name: 'Your GPUs, one reliable grid.' }).waitFor();
await page.getByRole('tab', { name: 'Nodes' }).click();
await page.getByText('Windows RTX Lab').waitFor();
await page.getByText('NVIDIA GeForce RTX 4090').waitFor();
await page.getByText('20 GiB free').waitFor();

await page.getByRole('tab', { name: 'Runs' }).click();
await page.getByText('1 GPU · 16 GiB free').waitFor();
const runRow = page.locator('[data-run]').first();
await runRow.focus();
await page.keyboard.press('Enter');
await page.locator('#inspector[aria-hidden="false"]').waitFor();
await page.locator('#inspector-body').getByText(/GPU-browser-4090/).waitFor();
await page.keyboard.press('Escape');
if (await page.locator('#inspector').getAttribute('aria-hidden') !== 'true') {
  throw new Error('Escape did not close the run inspector');
}

await page.getByRole('button', { name: 'New run' }).click();
await page.getByLabel('Experiment name').fill('UI serialized GPU request');
await page.getByLabel('GPU count').fill('1');
await page.getByLabel('Minimum free memory per GPU').fill('12');
await page.getByLabel('Required node labels').fill('os=windows,accelerator=nvidia');
await page.getByRole('button', { name: 'Submit run' }).click();
await page.getByText('1 GPU · 12 GiB free').waitFor({ timeout: 10000 });

await page.setViewportSize({ width: 390, height: 844 });
await page.getByRole('tab', { name: 'Nodes' }).click();
if (!await page.getByRole('tab', { name: 'Nodes' }).isVisible()) {
  throw new Error('Nodes tab is hidden at mobile width');
}
await page.getByText('Windows RTX Lab').waitFor();

if (errors.length) throw new Error(errors.join('\n'));
}
