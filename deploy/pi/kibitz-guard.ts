// kibitz-guard: the permission layer kibitz gives pi.
//
// pi has no permission model of its own: a tool it was given runs on whatever
// path the model names. OpenCode's permission block is what keeps kibitz's
// agent inside the checkout and its writes to the files a mode may write, and
// this is the same thing written as a tool_call handler (docs/agent-engine.md).
//
// It is the cheap layer, not the one that decides. The worker still checks
// every changed path after an implement run, and the container is still the
// boundary. What this stops is a prompt-injected "read /proc/self/environ" or
// "write ~/.pi/agent/settings.json" before it happens rather than after.
//
// The rules arrive in KIBITZ_PI_GUARD as JSON, written by the runner per job:
//
//	{"root": "/var/tmp/kibitz/ws-1",          // the checkout
//	 "tools": ["read", "grep", "find", "ls"], // tools the model may call
//	 "toolPrefixes": ["mcp__kibitz__"],       // and every tool of these servers
//	 "writable": "^(?:\\.kibitz/out/[^/]+)$"} // where write and edit may go
//
// Missing or unreadable rules block every call. A guard that failed open
// would be a guard that was never there.
import { realpathSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, isAbsolute, relative, resolve, sep } from "node:path";

type Rules = {
	root: string;
	tools: string[];
	toolPrefixes: string[];
	writable?: string;
};

type Block = { block: true; reason: string };

// The tools whose arguments name a path, and the argument that does. A
// missing path means the working directory, which is the root.
const pathArgument: Record<string, string> = {
	read: "path",
	grep: "path",
	find: "path",
	ls: "path",
	edit: "path",
	write: "path",
};

// Arguments that are globs evaluated under the path. They must not climb out
// of it.
const globArguments: Record<string, string[]> = {
	grep: ["glob"],
	find: ["pattern"],
};

const writing = new Set(["edit", "write"]);

function loadRules(): Rules | undefined {
	try {
		const rules = JSON.parse(process.env.KIBITZ_PI_GUARD ?? "") as Rules;
		if (typeof rules.root !== "string" || !isAbsolute(rules.root)) return undefined;
		rules.root = realpathSync(rules.root);
		rules.tools ??= [];
		rules.toolPrefixes ??= [];
		return rules;
	} catch {
		return undefined;
	}
}

// resolveLikePi mirrors how pi's file tools turn an argument into a path:
// unicode spaces become spaces, a leading "@" is dropped, "~" is the home
// directory, and anything relative is relative to the working directory.
function resolveLikePi(input: string, cwd: string): string {
	let path = input.replace(/[  -   　]/g, " ");
	if (path.startsWith("@")) path = path.slice(1);
	if (path === "~") path = homedir();
	else if (path.startsWith("~/")) path = homedir() + path.slice(1);
	return isAbsolute(path) ? resolve(path) : resolve(cwd, path);
}

// canonical resolves symbolic links in the longest part of path that exists.
// A file that is about to be written does not exist yet, but its directory
// does, and a link in the directory is what would carry the write elsewhere.
function canonical(path: string): string {
	let existing = path;
	const rest: string[] = [];
	for (;;) {
		try {
			return resolve(realpathSync(existing), ...rest);
		} catch {
			const parent = dirname(existing);
			if (parent === existing) return path;
			rest.unshift(existing.slice(parent.length + (parent.endsWith(sep) ? 0 : 1)));
			existing = parent;
		}
	}
}

// inside returns path relative to root, or undefined when it is not under it.
function inside(root: string, path: string): string | undefined {
	const rel = relative(root, path);
	if (rel === "") return ".";
	if (rel === ".." || rel.startsWith(".." + sep) || isAbsolute(rel)) return undefined;
	return rel.split(sep).join("/");
}

function check(rules: Rules | undefined, tool: string, input: Record<string, unknown>, cwd: string): Block | undefined {
	if (!rules) {
		return { block: true, reason: "kibitz-guard: no rules were given for this run, so no tool may be called" };
	}

	const allowed = rules.tools.includes(tool) || rules.toolPrefixes.some((prefix) => tool.startsWith(prefix));
	if (!allowed) {
		return { block: true, reason: `kibitz-guard: ${tool} is not available in this run` };
	}

	const argument = pathArgument[tool];
	if (!argument) return undefined;

	const raw = input?.[argument];
	if (raw !== undefined && typeof raw !== "string") {
		return { block: true, reason: `kibitz-guard: ${argument} must be a string` };
	}
	const rel = inside(rules.root, canonical(resolveLikePi(raw ?? ".", cwd)));
	if (rel === undefined) {
		return { block: true, reason: `kibitz-guard: ${raw} is outside the repository` };
	}

	for (const name of globArguments[tool] ?? []) {
		const glob = input?.[name];
		if (typeof glob !== "string") continue;
		if (isAbsolute(glob) || glob.startsWith("~") || glob.split(/[\\/]/).includes("..")) {
			return { block: true, reason: `kibitz-guard: ${name} must stay inside the repository` };
		}
	}

	if (writing.has(tool)) {
		let writable: RegExp | undefined;
		try {
			writable = rules.writable ? new RegExp(rules.writable) : undefined;
		} catch {
			writable = undefined;
		}
		if (!writable || !writable.test(rel)) {
			return { block: true, reason: `kibitz-guard: ${rel} may not be written in this run` };
		}
	}
	return undefined;
}

export default function (pi: any) {
	const rules = loadRules();
	pi.on("tool_call", async (event: any, ctx: any) => {
		return check(rules, event.toolName, event.input ?? {}, ctx?.cwd ?? process.cwd());
	});
}
