# VetGo Browser Agent Privacy Policy

Last updated: October 12, 2026

VetGo Browser Agent is a Chrome extension (built from the open-source Open
Browser Use project) that connects Chrome to a native messaging host installed
by Shipyard's chrome plugin on the same computer. It lets local AI agents use
a Chrome profile only after the user adds the extension to that profile, turns
it on there, and allows that profile in Shipyard.

## Data handled by the extension

The extension may access browser tabs, tab URLs, page content, browser history,
download metadata, browser interaction state, and data visible in pages that the
user or a paired local agent chooses to automate. Depending on the pages being
used, this can include personally identifiable information, authentication
information, personal communications, financial information, health information,
location information, web history, user activity, and website content.

## How data is used

Data is used only to provide the extension's core browser automation bridge:
inspecting browser state, controlling tabs, executing browser debugging commands,
forwarding browser events, and communicating with the paired local native host.

VetGo Browser Agent does not sell user data. VetGo Browser Agent does not use or
transfer user data for advertising, creditworthiness, lending, or purposes
unrelated to browser automation.

## Data transfer

The extension communicates only with the local native messaging host on the
user's device. The extension itself does not send data to a VetGo, Shipyard or
any other cloud service. A local agent connected to the native host may process
the browser data it requests, and may send it to the AI service that agent
uses, under that agent's own terms; users should only connect agents they
trust, and only allow profiles they would hand to such an agent.

## Data storage

The extension may store local operational state in Chrome extension storage,
such as session metadata needed to recover tab groups and native host status.
This data remains in the user's Chrome profile unless the user removes the
extension or clears browser extension data.

## Contact

For privacy or support questions, use the project issue tracker:

<https://github.com/tinhthanh/open-browser-use/issues>
