
const request = require('request');
const url = require('url');
const httpsAgent = require('./databox-https-agent.js');
const macaroonCache = require('./databox-macaroon-cache.js');
const fs = require('fs');
//
//Databox ENV vars
//
const DATABOX_ARBITER_ENDPOINT = process.env.DATABOX_ARBITER_ENDPOINT || "https://arbiter:8080";
const ARBITER_TOKEN   = fs.readFileSync("/run/secrets/CM_KEY",{encoding:'base64'});

/**
 * This module wraps the node request module https://github.com/request/request and adds:
 *
 * 1) an https agent that trust the container manger CA
 * 2) appropriate arbiter token when communicating with the arbiter
 * 3) Requests and caches macaroon form the arbiter before communicating with databox components other then the arbiter.
 *
 * @param {object} options a request option object (The only required option is uri)
 */
module.exports = function (options) {
    return new Promise((resolve,reject)=>{

        // TODO handle case where options is a string e.g https://www.some-url.com

        //
        // Workout the host and path of the request
        //
        const urlObject = url.parse(options.uri);
	    const path = urlObject.pathname;
	    const host = urlObject.hostname;
	    const protocol = urlObject.protocol;
	    const method = options.method || 'GET';

        //request to arbiter do not need a macaroon but do need the ARBITER_TOKEN
	    const isRequestToArbiter = DATABOX_ARBITER_ENDPOINT.indexOf(host) !== -1;

        //request to an external site or dev component
        //TODO: Lets not hard code these!!
	    const isExternalRequest = host.indexOf('.') !== -1;

	    const isExternalDevRequest = host.indexOf("app-server") !== -1 || host.indexOf("localhost") !== -1;

	    const isInternalUiRequest = path.indexOf("/ui") === 0;

        if(protocol === "https:") {
            //use the databox https agent
            options.agent = httpsAgent;
        }

        //set a 1000ms timeout
        options.timeout = 1000
        if(isRequestToArbiter) {
            options.headers = {'X-Api-Key': ARBITER_TOKEN};
            //do the request and call back when done
            //console.log("[databox-request] " + options.uri);
            resolve(request(options));
        
        } else if (isExternalDevRequest || isInternalUiRequest) {
            // we don't need a macaroon for DEV mode external request or UI requests.
            options.headers = {};
            console.log("[databox-request] ExternalRequest " + options.uri);
            resolve(request(options));
        } else if (isExternalRequest ) {
            //
            // we don't need a macaroon for an external request
            //
            // TODO::EXTERNAL REQUEST SHOULD BE ROOTED THROUGH THE DATABOX WHITELISTING PROXY THING (when its been written!!)
            options.headers = {};
            options.agent = undefined; //external request use the default agent.
            console.log("[databox-request] ExternalRequest " + options.uri);
            resolve(request(options));
        } else {
            //
            // we are talking to another databox component so we need a macaroon!
            //
            macaroonCache.getMacaroon(host,path, method)
            .then((macaroon)=>{
                //do the request and call back when done
                options.headers = {'X-Api-Key': macaroon};
                console.log("[databox-request-with-macaroon] ", options.uri);
                resolve(request(options));
            })
            .catch((result)=>{
                if(result.error !== null) {
                    console.log(result.error);
                    reject(result.error,result.response,null);
                    macaroonCache.invalidateMacaroon(host,path,method);
                    return;
                } else if (result.response.statusCode !== 200) {
                    //API responded with an error
                    console.log(result.body);
                    reject(result.body,result.response,null);
                    macaroonCache.invalidateMacaroon(host,path,method);
                    return;
                } else {
                    console.log(result.body, result.error);
                }
            });
        }
    });
};
