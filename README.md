# poc-dynamostreams

A proof of concept demonstrating DynamoDB Streams for cross-region data replication between DynamoDB instances.

## Overview

This project showcases how to use DynamoDB Streams to automatically replicate data changes across multiple DynamoDB tables. When changes occur in the source table, the stream captures these changes and triggers Lambda functions to replicate the data to target tables.

## Architecture
