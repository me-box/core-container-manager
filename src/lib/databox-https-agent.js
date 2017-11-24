
const https = require('https');
const fs = require('fs');
//
//Databox ENV vars
//
const CM_HTTPS_CA_ROOT_CERT = fs.readFileSync('/run/secrets/DATABOX_ROOT_CA') || process.env.CM_HTTPS_CA_ROOT_CERT || '';

//
// An https.Agent to trust the CM https cert if one is provided
//
const agentOptions = {};
if(CM_HTTPS_CA_ROOT_CERT === '') {
    console.log("WARNING[databox-request]:: no https root cert provided not checking https certs.");
    agentOptions.rejectUnauthorized = false;
} else {
    agentOptions.ca = CM_HTTPS_CA_ROOT_CERT;
}

const httpsAgent = new https.Agent(agentOptions);

module.exports = httpsAgent;