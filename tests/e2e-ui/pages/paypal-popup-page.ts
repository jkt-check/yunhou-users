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

  /** Selector for the approve/agree control on PayPal's review screen.
   *  The control varies by page vintage AND locale: modern checkout uses
   *  button#confirmButton, the hermes billing-agreement review renders
   *  input[type="submit"], and the buyer account's locale switches the
   *  text (observed: English login pages followed by a Chinese
   *  "同意并订阅" review button). Keep text variants for en + zh and
   *  structural fallbacks; always :visible so hidden twins don't match. */
  private static readonly APPROVE_SEL = [
    'button[data-testid="confirmButton"]:visible',
    'button#confirmButton:visible',
    'input#confirmButton:visible',
    'button:has-text("Agree & Subscribe"):visible',
    'button:has-text("同意并订阅"):visible',
    'button:has-text("Complete Purchase"):visible',
    'button:has-text("Subscribe"):visible',
    'button:has-text("订阅"):visible',
    'button:has-text("Agree"):visible',
    'button:has-text("同意"):visible',
    'button:has-text("Continue"):visible',
    'button:has-text("继续"):visible',
    'button:has-text("Pay Now"):visible',
    'input[type="submit"]:visible',
    'button[type="submit"]:visible',
  ].join(', ');

  /** Resolve the scope the login/approve form lives in: the popup
   *  document itself (real popup-window flow) or the embedded PayPal
   *  iframe. Returns null when neither shows a form (returning-customer
   *  path skips login entirely). */
  private async formScope(): Promise<Pick<Page, 'locator'> | null> {
    const emailSel = 'input[name="email"], input#email';
    const approveSel = PayPalPopupPage.APPROVE_SEL;
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
    // Click the LOGIN button specifically: PayPal's unified login keeps
    // both #btnNext and #btnLogin in the DOM (toggling visibility), so a
    // bare button[type="submit"] matches #btnNext first and submits the
    // email screen again, wedging the flow. :visible keeps the fallback
    // from matching the hidden one.
    await scope
      .locator('button#btnLogin:visible, button[type="submit"]:visible')
      .first()
      .click({ timeout: 30_000 });
    await this.page.waitForTimeout(2_000);
    return { loggedIn: true } as const;
  }

  async approve(): Promise<PayPalPopupResult> {
    // The review screen can take a while to appear after login (secure-
    // login spinner + several redirects in sandbox). Poll both the popup
    // document and the iframe variant for up to 60s instead of a single
    // snapshot check.
    const sel = PayPalPopupPage.APPROVE_SEL;
    const deadline = Date.now() + 60_000;
    for (;;) {
      const docBtn = this.page.locator(sel).first();
      if ((await docBtn.count()) > 0) {
        await docBtn.click({ timeout: 10_000 });
        await this.page.waitForTimeout(2_000);
        return { approved: true, flow: 'login-then-approve' as const };
      }
      try {
        const frameBtn = this.root().locator(sel).first();
        if ((await frameBtn.count()) > 0) {
          await frameBtn.click({ timeout: 10_000 });
          await this.page.waitForTimeout(2_000);
          return { approved: true, flow: 'login-then-approve' as const };
        }
      } catch {
        // iframe not mounted yet — keep polling.
      }
      if (Date.now() > deadline) {
        // Leave evidence of what the popup actually showed.
        await this.page
          .screenshot({ path: `test-results/approve-not-found-${Date.now()}.png` })
          .catch(() => {});
        throw new Error('approve button never appeared in popup');
      }
      await this.page.waitForTimeout(1_000);
    }
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
