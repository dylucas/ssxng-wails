export namespace api {
	
	export class Check {
	    name: string;
	    status: string;
	    detail: string;
	    suggestion: string;
	
	    static createFrom(source: any = {}) {
	        return new Check(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.status = source["status"];
	        this.detail = source["detail"];
	        this.suggestion = source["suggestion"];
	    }
	}
	export class HelperStatus {
	    path: string;
	    installed: boolean;
	    version: string;
	    requiredVersion: string;
	    versionOK: boolean;
	    setuid: boolean;
	    owner: string;
	    mode: string;
	    applied: string[];
	    lastError: string;
	
	    static createFrom(source: any = {}) {
	        return new HelperStatus(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.path = source["path"];
	        this.installed = source["installed"];
	        this.version = source["version"];
	        this.requiredVersion = source["requiredVersion"];
	        this.versionOK = source["versionOK"];
	        this.setuid = source["setuid"];
	        this.owner = source["owner"];
	        this.mode = source["mode"];
	        this.applied = source["applied"];
	        this.lastError = source["lastError"];
	    }
	}
	export class ListenerView {
	    address: string;
	    port: number;
	    active: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ListenerView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.address = source["address"];
	        this.port = source["port"];
	        this.active = source["active"];
	    }
	}
	export class LogEntry {
	    seq: number;
	    time: number;
	    level: string;
	    scope: string;
	    message: string;
	
	    static createFrom(source: any = {}) {
	        return new LogEntry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.seq = source["seq"];
	        this.time = source["time"];
	        this.level = source["level"];
	        this.scope = source["scope"];
	        this.message = source["message"];
	    }
	}
	export class NetworkService {
	    name: string;
	    hardwarePort: string;
	    device: string;
	    selected: boolean;
	    hasOurProxy: boolean;
	    webProxy: string;
	    socksProxy: string;
	    autoProxy: string;
	
	    static createFrom(source: any = {}) {
	        return new NetworkService(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.hardwarePort = source["hardwarePort"];
	        this.device = source["device"];
	        this.selected = source["selected"];
	        this.hasOurProxy = source["hasOurProxy"];
	        this.webProxy = source["webProxy"];
	        this.socksProxy = source["socksProxy"];
	        this.autoProxy = source["autoProxy"];
	    }
	}
	export class PACView {
	    address: string;
	    port: number;
	    active: boolean;
	    url: string;
	    ruleCount: number;
	    generatedAt: number;
	    externalURL: string;
	    source: string;
	    lastError: string;
	
	    static createFrom(source: any = {}) {
	        return new PACView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.address = source["address"];
	        this.port = source["port"];
	        this.active = source["active"];
	        this.url = source["url"];
	        this.ruleCount = source["ruleCount"];
	        this.generatedAt = source["generatedAt"];
	        this.externalURL = source["externalURL"];
	        this.source = source["source"];
	        this.lastError = source["lastError"];
	    }
	}
	export class SelfTestStep {
	    name: string;
	    ok: boolean;
	    detail: string;
	
	    static createFrom(source: any = {}) {
	        return new SelfTestStep(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.name = source["name"];
	        this.ok = source["ok"];
	        this.detail = source["detail"];
	    }
	}
	export class SelfTestReport {
	    ok: boolean;
	    steps: SelfTestStep[];
	    elapsedMs: number;
	
	    static createFrom(source: any = {}) {
	        return new SelfTestReport(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ok = source["ok"];
	        this.steps = this.convertValues(source["steps"], SelfTestStep);
	        this.elapsedMs = source["elapsedMs"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	
	export class ServerView {
	    id: string;
	    host: string;
	    port: number;
	    method: string;
	    password: string;
	    remark: string;
	    plugin: string;
	    title: string;
	    hasPlugin: boolean;
	    active: boolean;
	    connected: boolean;
	    lastError: string;
	
	    static createFrom(source: any = {}) {
	        return new ServerView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.host = source["host"];
	        this.port = source["port"];
	        this.method = source["method"];
	        this.password = source["password"];
	        this.remark = source["remark"];
	        this.plugin = source["plugin"];
	        this.title = source["title"];
	        this.hasPlugin = source["hasPlugin"];
	        this.active = source["active"];
	        this.connected = source["connected"];
	        this.lastError = source["lastError"];
	    }
	}
	export class SettingsView {
	    socksAddress: string;
	    socksPort: number;
	    httpEnabled: boolean;
	    httpAddress: string;
	    httpPort: number;
	    httpFollowGlobal: boolean;
	    udpEnabled: boolean;
	    pacAddress: string;
	    pacPort: number;
	    pacBindLocalhost: boolean;
	    gfwlistURL: string;
	    externalPACURL: string;
	    exceptions: string;
	    autoConfigureNetworkServices: boolean;
	    networkServices: string[];
	    enablePAC: boolean;
	    enableGlobal: boolean;
	    enableManual: boolean;
	    enableExternalPAC: boolean;
	    launchAtLogin: boolean;
	    hideDockIcon: boolean;
	    clearProxyOnQuit: boolean;
	    restoreProxyOnWake: boolean;
	
	    static createFrom(source: any = {}) {
	        return new SettingsView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.socksAddress = source["socksAddress"];
	        this.socksPort = source["socksPort"];
	        this.httpEnabled = source["httpEnabled"];
	        this.httpAddress = source["httpAddress"];
	        this.httpPort = source["httpPort"];
	        this.httpFollowGlobal = source["httpFollowGlobal"];
	        this.udpEnabled = source["udpEnabled"];
	        this.pacAddress = source["pacAddress"];
	        this.pacPort = source["pacPort"];
	        this.pacBindLocalhost = source["pacBindLocalhost"];
	        this.gfwlistURL = source["gfwlistURL"];
	        this.externalPACURL = source["externalPACURL"];
	        this.exceptions = source["exceptions"];
	        this.autoConfigureNetworkServices = source["autoConfigureNetworkServices"];
	        this.networkServices = source["networkServices"];
	        this.enablePAC = source["enablePAC"];
	        this.enableGlobal = source["enableGlobal"];
	        this.enableManual = source["enableManual"];
	        this.enableExternalPAC = source["enableExternalPAC"];
	        this.launchAtLogin = source["launchAtLogin"];
	        this.hideDockIcon = source["hideDockIcon"];
	        this.clearProxyOnQuit = source["clearProxyOnQuit"];
	        this.restoreProxyOnWake = source["restoreProxyOnWake"];
	    }
	}
	export class Traffic {
	    up: number;
	    down: number;
	    upSpeed: number;
	    downSpeed: number;
	    active: number;
	    sessions: number;
	
	    static createFrom(source: any = {}) {
	        return new Traffic(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.up = source["up"];
	        this.down = source["down"];
	        this.upSpeed = source["upSpeed"];
	        this.downSpeed = source["downSpeed"];
	        this.active = source["active"];
	        this.sessions = source["sessions"];
	    }
	}
	export class State {
	    version: string;
	    enabled: boolean;
	    mode: string;
	    modes: string[];
	    socks: ListenerView;
	    http: ListenerView;
	    udp: ListenerView;
	    pac: PACView;
	    server?: ServerView;
	    servers: ServerView[];
	    traffic: Traffic;
	    helper: HelperStatus;
	    settings: SettingsView;
	    lastError: string;
	    startedAt: number;
	
	    static createFrom(source: any = {}) {
	        return new State(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.version = source["version"];
	        this.enabled = source["enabled"];
	        this.mode = source["mode"];
	        this.modes = source["modes"];
	        this.socks = this.convertValues(source["socks"], ListenerView);
	        this.http = this.convertValues(source["http"], ListenerView);
	        this.udp = this.convertValues(source["udp"], ListenerView);
	        this.pac = this.convertValues(source["pac"], PACView);
	        this.server = this.convertValues(source["server"], ServerView);
	        this.servers = this.convertValues(source["servers"], ServerView);
	        this.traffic = this.convertValues(source["traffic"], Traffic);
	        this.helper = this.convertValues(source["helper"], HelperStatus);
	        this.settings = this.convertValues(source["settings"], SettingsView);
	        this.lastError = source["lastError"];
	        this.startedAt = source["startedAt"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}

}

