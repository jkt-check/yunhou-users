/**
 * PayPalPopupPage — represents the PayPal sandbox login/payment popup.
 *
 * PayPal SDK opens either an iframe (preferred) or a new window/popup
 * depending on the device flow. We handle both via Playwright's
 * `page.on('popup', ...)` event + `frameLocator`.
 */

import { FrameLocator, Locator, Page, expect } from '@playwright/test';
import { SandboxEnv } from '../helpers/env';

export interface PayPalPopupResult {
  /** True if approval button clicked successfully; false if cancelled. */
  approved: boolean;
  /** Path through the popup that was hit. */
  flow: 'login-then-approve' | 'already-logged-in-approve' | 'declined' | 'cancelled';
}

export class PayPalPopupPage {
  // Lazily resolved — the iframe doesn't exist at construction time
  // (PayPal SDK loads it asynchronously after script execution).
  readonly root: () => FrameLocator;
  readonly page: Page;

  constructor(page: Page, _env: SandboxEnv) {
    this.page = page;
    this.root = () => page.frameLocator('iframe[name^="__paypal"], iframe[src*="paypal"], iframe[id^="__paypal"]').first();
  }

  /** Some tests prefer a real popup window over an iframe. */
  async waitForPopup(): Promise<Page> {
    const popup = await this.page.waitForEvent('popup', { timeout: 30_000 });
    return popup;
  }

  /** Resolve the scope the login/approve form lives in: the popup
   *  document itself (real popup-window flow) or the embedded PayPal
   *  iframe. Returns null when neither shows a form (returning-customer
   *  path skips login entirely). */
  private async formScope(): Promise<Pick<Page, 'locator'> | null> {
    const emailSel = 'input[name="email"], input#email';
    const approveSel =
      'button[data-testid="confirmButton"], button#confirmButton, button:has-text("Subscribe"), button:has-text("Continue"), button:has-text("Agree"), button[type="submit"]';
    try {
      await this.page
        .locator(`${emailSel}, ${approveSel}`)
        .first()
        .waitFor({ state: 'visible', timeout: 10_000 });
      return this.page;
    } catch {
      try {
        await this.root()
          .locator(`${emailSel}, ${approveSel}`)
          .first()
          .waitFor({ state: 'visible', timeout: 10_000 });
        return this.root();
      } catch {
        return null;
      }
    }
  }

  async loginIfNeeded(env: SandboxEnv) {
    // PayPal's "returning customer" path may skip the login form. Detect
    // and adapt: if email field is present, fill it; otherwise skip.
    const scope = await this.formScope();
    if (!scope) {
      return { loggedIn: true } as const;
    }
    const emailInput = scope.locator('input[name="email"], input#email').first();
    if ((await emailInput.count()) === 0 || !(await emailInput.isVisible().catch(() => false))) {
      // Approve screen is already up — no login required.
      return { loggedIn: true } as const;
    }
    await emailInput.fill(env.buyerEmail);
    const passwordInput = scope.locator('input[name="password"], input#password').first();
    if (!(await passwordInput.isVisible().catch(() => false))) {
      // Split login: email → "Next" → password on a second screen.
      const next = scope.locator('button#btnNext, button:has-text("Next"), button:has-text("下一步")').first();
      if ((await next.count()) > 0) {
        await next.click();
      }
    }
    await passwordInput.waitFor({ state: 'visible', timeout: 15_000 });
    await passwordInput.fill(env.buyerPassword);
    await scope.locator('button[type="submit"], button#btnLogin').first().click();
    await this.page.waitForTimeout(2_000);
    return { loggedIn: true } as const;
  }

  async approve(): Promise<PayPalPopupResult> {
    const scope = (await this.formScope()) ?? this.page;
    const approveBtn = scope.locator(
      'button[data-testid="confirmButton"], button#confirmButton, button:has-text("Subscribe"), button:has-text("Continue"), button:has-text("Agree & Subscribe"), button:has-text("Complete Purchase"), button:has-text("Pay")',
    );
    const count = await approveBtn.count();
    if (count === 0) {
      await scope.locator('button[type="submit"]').first().click();
    } else {
      await approveBtn.first().click();
    }
    await this.page.waitForTimeout(2_000);
    return { approved: true, flow: 'login-then-approve' as const };
  }

  async decline(): Promise<PayPalPopupResult> {
    const cancel = this.root().locator(
      'button:has-text("Cancel"), a:has-text("Cancel"), button:has-text("Go back")',
    ).first();
    const count = await cancel.count();
    if (count === 0) {
      await this.page.keyboard.press('Escape');
    } else {
      await cancel.click();
    }
    return { approved: false, flow: 'cancelled' };
  }
}
